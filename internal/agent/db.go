package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
)

// DB is the loop's checkpoint store. Every write takes a share lock on the
// attempt row and compares the fence in the same transaction, so a superseded
// runner cannot add a message or move a tool call (AC-5).
type DB struct {
	Pool *pgxpool.Pool
	// BeforeCommit is a test seam: crash injection inside a commit.
	BeforeCommit func(point string) error
}

var ErrRestartCap = errors.New("restart cap reached")

type Message struct {
	Idx        int
	Role       string
	Content    []byte
	RequestSHA string
	StopReason string
	CostMicros int64
}

type ToolCall struct {
	ID, Name      string
	MsgIdx        int
	Input         []byte
	SideEffecting bool
	State         string
	Result        []byte
	IsError       bool
}

type RunRow struct {
	Elapsed     time.Duration
	Starts      int
	BillingMode string
	Outcome     string
	OutcomeNote string
}

func (d *DB) fenced(ctx context.Context, attempt domain.AttemptID, fence int64, f func(tx pgx.Tx) error) error {
	tx, err := d.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	var cur int64
	var state string
	err = tx.QueryRow(ctx, `SELECT fence, state FROM attempts WHERE id = $1 FOR SHARE`, string(attempt)).Scan(&cur, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return err
	}
	if cur != fence {
		return fmt.Errorf("%w: attempt %s is at fence %d, runner holds %d", domain.ErrStaleFence, attempt, cur, fence)
	}
	switch domain.AttemptState(state) {
	case domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning:
	default:
		return fmt.Errorf("%w: attempt %s is %s", domain.ErrInvalidTransition, attempt, state)
	}
	if err := f(tx); err != nil {
		return err
	}
	if d.BeforeCommit != nil {
		if err := d.BeforeCommit("commit"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// StartRun registers a (re)start and returns the new start count. The restart
// cap parks a crash loop before it can spend again.
func (d *DB) StartRun(ctx context.Context, attempt domain.AttemptID, fence int64, billingMode string, cap int) (int, error) {
	var starts int
	err := d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO agent_runs (attempt_id, fence, billing_mode, starts) VALUES ($1, $2, $3, 1)
			ON CONFLICT (attempt_id) DO UPDATE SET starts = agent_runs.starts + 1
			WHERE agent_runs.fence = $2 AND agent_runs.outcome IS NULL
			RETURNING starts`, string(attempt), fence, billingMode).Scan(&starts)
		if errors.Is(err, pgx.ErrNoRows) {
			// Finished or fenced differently: report the stored row, do not count.
			return tx.QueryRow(ctx, `SELECT starts FROM agent_runs WHERE attempt_id = $1`, string(attempt)).Scan(&starts)
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	if cap > 0 && starts > cap {
		return starts, fmt.Errorf("%w: %d starts, cap %d", ErrRestartCap, starts, cap)
	}
	return starts, nil
}

func (d *DB) Run(ctx context.Context, attempt domain.AttemptID) (RunRow, error) {
	var r RunRow
	var o, n *string
	var secs float64
	err := d.Pool.QueryRow(ctx, `SELECT starts, billing_mode, outcome, outcome_note, extract(epoch FROM now() - created_at)::float8 FROM agent_runs WHERE attempt_id = $1`, string(attempt)).Scan(&r.Starts, &r.BillingMode, &o, &n, &secs)
	r.Elapsed = time.Duration(secs * float64(time.Second))
	if o != nil {
		r.Outcome = *o
	}
	if n != nil {
		r.OutcomeNote = *n
	}
	return r, err
}

// Finish records the terminal outcome once.
func (d *DB) Finish(ctx context.Context, attempt domain.AttemptID, fence int64, outcome, note string) error {
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_runs SET outcome = $3, outcome_note = $4, ended_at = now()
			WHERE attempt_id = $1 AND fence = $2 AND outcome IS NULL`, string(attempt), fence, outcome, note)
		return err
	})
}

// Messages loads the conversation in order.
func (d *DB) Messages(ctx context.Context, attempt domain.AttemptID) ([]Message, error) {
	rows, err := d.Pool.Query(ctx, `SELECT idx, role, content, coalesce(request_sha,''), coalesce(stop_reason,''), coalesce(cost_micros,0)
		FROM agent_messages WHERE attempt_id = $1 ORDER BY idx`, string(attempt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Idx, &m.Role, &m.Content, &m.RequestSHA, &m.StopReason, &m.CostMicros); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) ToolCalls(ctx context.Context, attempt domain.AttemptID) ([]ToolCall, error) {
	rows, err := d.Pool.Query(ctx, `SELECT tool_use_id, name, msg_idx, input, side_effecting, state, coalesce(result,''::bytea), coalesce(is_error,false)
		FROM agent_tool_calls WHERE attempt_id = $1 ORDER BY msg_idx, tool_use_id`, string(attempt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var c ToolCall
		if err := rows.Scan(&c.ID, &c.Name, &c.MsgIdx, &c.Input, &c.SideEffecting, &c.State, &c.Result, &c.IsError); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendUser adds a user message at idx (which must be the next index).
func (d *DB) AppendUser(ctx context.Context, attempt domain.AttemptID, fence int64, idx int, content []byte) error {
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		if err := nextIdx(ctx, tx, attempt, idx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_messages (attempt_id, idx, role, content, fence) VALUES ($1, $2, 'user', $3, $4)`,
			string(attempt), idx, content, fence)
		return err
	})
}

// AssistantTurn is a completed model response to checkpoint.
type AssistantTurn struct {
	Content      []byte
	RequestSHA   string
	RequestIDs   []string
	StopReason   string
	Model        string
	UsageJSON    []byte
	AttemptsJSON []byte
	CostMicros   int64
	CostLabel    string
	Calls        []ToolCall
}

// CommitAssistant stores the response and its tool calls (PENDING) in one
// transaction under the fence. Tools run only after this commits.
func (d *DB) CommitAssistant(ctx context.Context, attempt domain.AttemptID, fence int64, idx int, t AssistantTurn) error {
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		if err := nextIdx(ctx, tx, attempt, idx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha, request_ids, stop_reason, model, usage, cost_micros, cost_label, attempts)
			VALUES ($1, $2, 'assistant', $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12::jsonb)`,
			string(attempt), idx, t.Content, fence, t.RequestSHA, t.RequestIDs, t.StopReason, t.Model, t.UsageJSON, t.CostMicros, t.CostLabel, t.AttemptsJSON); err != nil {
			return err
		}
		for _, c := range t.Calls {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_tool_calls (attempt_id, tool_use_id, msg_idx, name, input, side_effecting, fence)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, string(attempt), c.ID, idx, c.Name, c.Input, c.SideEffecting, fence); err != nil {
				return err
			}
		}
		return nil
	})
}

func nextIdx(ctx context.Context, tx pgx.Tx, attempt domain.AttemptID, idx int) error {
	var next int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(idx) + 1, 0) FROM agent_messages WHERE attempt_id = $1`, string(attempt)).Scan(&next); err != nil {
		return err
	}
	if next != idx {
		return fmt.Errorf("%w: message index %d, next is %d", domain.ErrInvalidTransition, idx, next)
	}
	return nil
}

// MarkExecuting moves PENDING -> EXECUTING before the tool runs. Zero rows
// means another runner or an earlier life already started it.
func (d *DB) MarkExecuting(ctx context.Context, attempt domain.AttemptID, fence int64, id string) error {
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE agent_tool_calls SET state = 'EXECUTING', started_at = now()
			WHERE attempt_id = $1 AND tool_use_id = $2 AND state = 'PENDING' AND fence = $3`, string(attempt), id, fence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: tool call %s is not PENDING", domain.ErrInvalidTransition, id)
		}
		return nil
	})
}

// FinishTool records a result (DONE) or a synthetic one (INTERRUPTED).
func (d *DB) FinishTool(ctx context.Context, attempt domain.AttemptID, fence int64, id, state string, result []byte, isErr bool) error {
	if state != "DONE" && state != "INTERRUPTED" {
		return fmt.Errorf("bad terminal state %q", state)
	}
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE agent_tool_calls SET state = $3, result = $4, is_error = $5, ended_at = now()
			WHERE attempt_id = $1 AND tool_use_id = $2 AND state = 'EXECUTING'`, string(attempt), id, state, result, isErr)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: tool call %s is not EXECUTING", domain.ErrInvalidTransition, id)
		}
		return nil
	})
}

// AppendToolResults commits the user message that carries tool results, only
// when every tool call of the previous assistant message is terminal.
func (d *DB) AppendToolResults(ctx context.Context, attempt domain.AttemptID, fence int64, idx int, content []byte) error {
	return d.fenced(ctx, attempt, fence, func(tx pgx.Tx) error {
		if err := nextIdx(ctx, tx, attempt, idx); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE attempt_id = $1 AND msg_idx = $2 AND state NOT IN ('DONE','INTERRUPTED')`,
			string(attempt), idx-1).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return fmt.Errorf("%w: %d tool calls of message %d are unresolved", domain.ErrInvalidTransition, open, idx-1)
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_messages (attempt_id, idx, role, content, fence) VALUES ($1, $2, 'user', $3, $4)`,
			string(attempt), idx, content, fence)
		return err
	})
}

// TokensUsed sums input and output tokens over the attempt's responses.
func (d *DB) TokensUsed(ctx context.Context, attempt domain.AttemptID) (int64, error) {
	var n int64
	err := d.Pool.QueryRow(ctx, `SELECT coalesce(sum(coalesce((usage->>'input_tokens')::bigint,0) + coalesce((usage->>'output_tokens')::bigint,0)
		+ coalesce((usage->>'cache_creation_input_tokens')::bigint,0) + coalesce((usage->>'cache_read_input_tokens')::bigint,0)),0)
		FROM agent_messages WHERE attempt_id = $1 AND role = 'assistant'`, string(attempt)).Scan(&n)
	return n, err
}
