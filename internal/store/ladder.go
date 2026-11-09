package store

// goodqa#15324 S1b: ladder records through Execute. The ladder mission row is
// created inside the first record's transaction, so a refused first record
// leaves no row, and a ladder row never has zero events. No migration: the
// mission is an ordinary anthropic-api IDLE row that nothing ever admits.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/envelope"
	"fabric/internal/ladder"
	"fabric/internal/reducer"
)

// ladderMaxReceipts is the per-task receipt cap (escalation.py MAX_EVENTS).
// A package variable only so a test can lower it; production is
// domain.MaxLadderReceipts (checked by a test).
var ladderMaxReceipts = domain.MaxLadderReceipts

// ladderMaxSpends bounds spend records per task; they do not use receipt slots
// (escalation.py add_spend adds no event). A variable only for tests.
var ladderMaxSpends = domain.MaxLadderSpends

// reserveLadderPrefix keeps the m_ladder- namespace for ladder records: no
// operator, importer or native start can create such a mission.
func reserveLadderPrefix(id domain.MissionID) error {
	if domain.IsLadderMission(id) {
		return fmt.Errorf("%w: the %s prefix is reserved for ladder records", domain.ErrInvalidID, domain.LadderMissionPrefix)
	}
	return nil
}

// LadderCommandID derives the idempotency key (PL-7, PL2-1). The task is part
// of the mission id, so the key is scoped to task plus content.
//   - receipts: "r" + first 62 hex of receipt_sha256, so a resend is a replay and
//     any change to the same receipt is a conflict;
//   - spend: "s" + sha256 of the whole body, which includes the per-task source
//     ordinal, so two identical add_spend calls are two distinct spends and a
//     resend of the SAME ordinal is a replay;
//   - claim and approval: "c" + sha256 of the whole body.
func LadderCommandID(r *domain.LadderRecord) (domain.CommandID, error) {
	if err := domain.ValidateLadderRecord(r); err != nil {
		return "", err
	}
	if r.Rsha != nil {
		return domain.CommandID("r" + (*r.Rsha)[:62]), nil
	}
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	prefix := "c"
	if r.Step == "spend" {
		prefix = "s"
	}
	return domain.CommandID(prefix + hex.EncodeToString(sum[:])[:62]), nil
}

// RecordLadderReceipt appends one classified record to the task's ladder
// mission under the fixed ladder actor. The first record creates the row.
func (s *Store) RecordLadderReceipt(ctx context.Context, task string, r domain.LadderRecord) (Response, error) {
	id, err := domain.LadderMissionID(task)
	if err != nil {
		return Response{}, err
	}
	key, err := LadderCommandID(&r)
	if err != nil {
		return Response{}, err
	}
	return s.Execute(ctx, domain.LadderActor, key, domain.Command{Kind: domain.CmdRecordLadderReceipt, Mission: id, Ladder: &r})
}

// ladderPrepare runs before the mission row lock. For the first record of a
// task it checks the shape (pure, so a refusal stores nothing) and inserts the
// row in this transaction. It also fixes the one writer.
func ladderPrepare(ctx context.Context, tx pgx.Tx, actor domain.ActorID, cmd domain.Command) error {
	if actor != domain.LadderActor {
		return fmt.Errorf("%w: ladder records are written by the ladder actor only", domain.ErrInvalidTransition)
	}
	// CI-3: only an id the builder would make: the prefix plus a SAFE_KEY task.
	if !domain.ValidLadderMissionID(cmd.Mission) {
		return fmt.Errorf("%w: not a ladder mission id", domain.ErrInvalidID)
	}
	if _, err := reducer.Decide(domain.NewMission(cmd.Mission), cmd, reducer.Env{}); err != nil {
		return err
	}
	state, err := json.Marshal(domain.NewMission(cmd.Mission))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO missions (id, ticket, repo, branch, state, provider)
		VALUES ($1, $2, '', '', $3, 'anthropic-api') ON CONFLICT (id) DO NOTHING`, string(cmd.Mission), domain.LadderTicket, state)
	return err
}

// ladderGuard runs after the row lock, the replay lookup and the foreign-kind
// check, so it sees the locked state. It returns a stored response when the
// receipt is already in the log (whatever actor or key recorded it), or an
// error that stores nothing.
func ladderGuard(ctx context.Context, tx pgx.Tx, cmd domain.Command, m domain.Mission) (*Response, error) {
	var provider, ticket, repo string
	if err := tx.QueryRow(ctx, "SELECT provider, ticket, repo FROM missions WHERE id = $1", string(cmd.Mission)).Scan(&provider, &ticket, &repo); err != nil {
		return nil, err
	}
	if provider != string(domain.ProviderAnthropicAPI) || ticket != domain.LadderTicket || repo != "" {
		return nil, fmt.Errorf("%w: %s exists and is not a ladder mission", domain.ErrInvalidID, cmd.Mission)
	}
	r := cmd.Ladder
	// A receipt is identified by its sha256, a spend by its per-task source
	// ordinal (PL2-1): the same identity with the same body replays, with
	// another body conflicts, whatever actor or key recorded it first.
	var ident, identVal string
	switch {
	case r.Rsha != nil:
		ident, identVal = "rsha", *r.Rsha
	case r.Step == "spend":
		ident, identVal = "ord", fmt.Sprint(r.Ordinal)
	}
	if ident != "" {
		var seq int64
		var payload []byte
		err := tx.QueryRow(ctx, `SELECT seq, payload FROM events WHERE mission_id = $1 AND kind = 'LADDER_RECEIPT'
			AND payload->'ladder'->>($2::text) = $3::text ORDER BY seq LIMIT 1`, string(cmd.Mission), ident, identVal).Scan(&seq, &payload)
		switch {
		case err == nil:
			stored, err := envelope.DecodeEvent(payload)
			if err != nil {
				return nil, err
			}
			a, _ := json.Marshal(stored.Ladder)
			b, _ := json.Marshal(r)
			if string(a) != string(b) {
				return nil, fmt.Errorf("%w: receipt already recorded with a different body", domain.ErrIdempotencyConflict)
			}
			return &Response{Status: "ok", Events: []domain.Event{stored}, FirstSeq: seq, LastSeq: seq, Version: m.Version, Replayed: true}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, err
		}
	}
	// Three separate bounds (CI-2, S1B-A2). Receipts: LADDER_RECEIPT events that
	// are not spend records, like escalation.py MAX_EVENTS. Spends: their own
	// bound, refused with ErrPolicy (escalation.py add_spend adds no event and
	// has no cap). Approvals: LADDER_TOP_APPROVED.
	kind, limit := domain.LadderEventKind(r), ladderMaxReceipts
	what := "receipt"
	q := "SELECT count(*) FROM events WHERE mission_id = $1 AND kind = $2 AND COALESCE(payload->'ladder'->>'step', '') <> 'spend'"
	switch {
	case kind == domain.EvLadderTopApproved:
		limit, what = domain.MaxLadderApprovals, "approval"
		q = "SELECT count(*) FROM events WHERE mission_id = $1 AND kind = $2"
	case r.Step == "spend":
		limit, what = ladderMaxSpends, "spend"
		q = "SELECT count(*) FROM events WHERE mission_id = $1 AND kind = $2 AND payload->'ladder'->>'step' = 'spend'"
	}
	var n int
	if err := tx.QueryRow(ctx, q, string(cmd.Mission), string(kind)).Scan(&n); err != nil {
		return nil, err
	}
	if n >= limit {
		if what == "spend" {
			return nil, fmt.Errorf("%w: %w: task already holds %d spend records (limit %d); an operator must escalate", ladder.ErrPolicy, domain.ErrTooManyReceipts, n, limit)
		}
		return nil, fmt.Errorf("%w: task already holds %d %s records (limit %d); an operator must escalate", domain.ErrTooManyReceipts, n, what, limit)
	}
	return nil, nil
}

// LadderEvents returns a task's ladder log in one READ ONLY snapshot. It reads
// at most cap+approvals+1 rows and refuses a longer log; it never truncates.
func (s *Store) LadderEvents(ctx context.Context, task string) ([]domain.Event, error) {
	id, err := domain.LadderMissionID(task)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // read-only
	return ReadLadderEvents(ctx, tx, id)
}

// ReadLadderEvents reads inside a caller's READ ONLY transaction, so the
// status command can use the read-only pool.
func ReadLadderEvents(ctx context.Context, tx pgx.Tx, id domain.MissionID) ([]domain.Event, error) {
	max := ladderMaxReceipts + ladderMaxSpends + domain.MaxLadderApprovals
	rows, err := tx.Query(ctx, `SELECT payload FROM events WHERE mission_id = $1 ORDER BY seq LIMIT $2`, string(id), max+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		e, err := envelope.DecodeEvent(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("%w: more than %d events for %s", domain.ErrTooManyReceipts, max, id)
	}
	n, spends := 0, 0
	for _, e := range out {
		switch {
		case e.Kind != domain.EvLadderReceipt || e.Ladder == nil:
		case e.Ladder.Step == "spend":
			spends++
		default:
			n++
		}
	}
	if n > ladderMaxReceipts {
		return nil, fmt.Errorf("%w: more than %d receipts for %s", domain.ErrTooManyReceipts, ladderMaxReceipts, id)
	}
	if spends > ladderMaxSpends {
		return nil, fmt.Errorf("%w: %w: more than %d spend records for %s", ladder.ErrPolicy, domain.ErrTooManyReceipts, ladderMaxSpends, id)
	}
	return out, nil
}
