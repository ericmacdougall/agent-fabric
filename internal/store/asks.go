package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// askID bounds an ask id so that its wake key "ask:<id>" is itself a valid
// command id (domain.ParseCommandID, 63-character body).
var askID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,47}$`)

// AskActor is the actor every ask wake is recorded under. With the
// deterministic key AskWakeKey it makes the wake idempotent per ask.
const AskActor = "asks"

// AskWakeKey is the idempotency key of the wake an answer fires.
func AskWakeKey(id string) string { return "ask:" + id }

// AskToken is the wait token a mission names to wait on an ask.
func AskToken(id string) string { return askTokenPrefix + id }

const askTokenPrefix = "ask:"

func isAskToken(token string) bool { return strings.HasPrefix(token, askTokenPrefix) }

// checkAskReserved keeps the ask: wait tokens to the asks actor (review F1).
// AskActor records only ask wakes, each under its fixed key, and may
// acknowledge them; no other actor may WAKE or ACK_WAKE an ask: token.
// FABRIC_ACTOR can name any actor, so Execute also requires the ask to be
// answered for the mission before an ask wake is decided.
// SchedulerActor is the only actor that may mark an abandonment as
// pre-launch or as a capacity wait (red-team N3); 0015 backs it by role.
const SchedulerActor domain.ActorID = "fabd"

func checkSchedulerReserved(actor domain.ActorID, cmd domain.Command) error {
	if (cmd.Prelaunch || cmd.CapacityWait) && actor != SchedulerActor {
		return fmt.Errorf("%w: pre-launch and capacity-wait finishes are reserved to actor %s", domain.ErrInvalidTransition, SchedulerActor)
	}
	return nil
}

func checkAskReserved(actor domain.ActorID, commandID domain.CommandID, cmd domain.Command) error {
	wakeKind := cmd.Kind == domain.CmdWake || cmd.Kind == domain.CmdAckWake
	askTok := wakeKind && isAskToken(cmd.Token)
	switch {
	case actor == AskActor && !askTok:
		return fmt.Errorf("%w: actor %s records only ask wakes", domain.ErrInvalidTransition, AskActor)
	case askTok && actor != AskActor:
		return fmt.Errorf("%w: %s %s is reserved to actor %s", domain.ErrInvalidTransition, cmd.Kind, cmd.Token, AskActor)
	case askTok && cmd.Kind == domain.CmdWake && string(commandID) != AskWakeKey(strings.TrimPrefix(cmd.Token, askTokenPrefix)):
		return fmt.Errorf("%w: an ask wake uses key %s", domain.ErrInvalidTransition, AskWakeKey(strings.TrimPrefix(cmd.Token, askTokenPrefix)))
	}
	return nil
}

// AnswerResult says what an answer did.
type AnswerResult struct {
	Ask      Ask
	Replayed bool      // the same answer was already recorded
	Wake     *Response // nil when the ask names no mission
}

// AnswerAsk records the answer to an open ask and fires WAKE ask:<id> on its
// mission (AC-10). The answer needs a grammatical source (R12).
//
// The answer row and the wake are two transactions. The wake runs under the
// fixed key (AskActor, AskWakeKey(id)), so a crash between them is repaired by
// repeating the same answer or by RepairAskWakes, and can never wake twice.
// The same answer again replays; a different answer to an answered ask is
// ErrIdempotencyConflict; a withdrawn ask cannot be answered.
func (s *Store) AnswerAsk(ctx context.Context, id, answer, by, source string) (AnswerResult, error) {
	if !askID.MatchString(id) {
		return AnswerResult{}, fmt.Errorf("%w: ask id", domain.ErrInvalidID)
	}
	if answer == "" || len(answer) > 4096 {
		return AnswerResult{}, fmt.Errorf("%w: an answer must be 1..4096 bytes", domain.ErrInvalidTransition)
	}
	if _, err := domain.ParseActorID(by); err != nil {
		return AnswerResult{}, fmt.Errorf("answered by: %w", err)
	}
	if !ValidSource(source) {
		return AnswerResult{}, fmt.Errorf("%w: answer to %s has no grammatical source (R12): %q", domain.ErrInvalidTransition, id, source)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AnswerResult{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	var state, mission, prevAnswer, prevSource string
	err = tx.QueryRow(ctx, `SELECT state, COALESCE(mission_id, ''), COALESCE(answer, ''), COALESCE(answer_source, '')
		FROM asks WHERE id = $1 FOR UPDATE`, id).Scan(&state, &mission, &prevAnswer, &prevSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnswerResult{}, fmt.Errorf("%w: ask %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return AnswerResult{}, err
	}
	res := AnswerResult{}
	switch state {
	case "withdrawn":
		return AnswerResult{}, fmt.Errorf("%w: ask %s was withdrawn", domain.ErrInvalidTransition, id)
	case "answered":
		if prevAnswer != answer || prevSource != source {
			return AnswerResult{}, fmt.Errorf("%w: ask %s already answered differently", domain.ErrIdempotencyConflict, id)
		}
		res.Replayed = true
	default:
		if _, err := tx.Exec(ctx, `UPDATE asks SET state = 'answered', answer = $2, answered_by = $3,
			answered_at = now(), answer_source = $4 WHERE id = $1`, id, answer, by, source); err != nil {
			return AnswerResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AnswerResult{}, err
	}
	if mission != "" {
		w, err := s.wakeForAsk(ctx, id, domain.MissionID(mission))
		if err != nil {
			// Review F8: the answer is committed; report it with the stored
			// ask, and never as a refusal ("nothing changed").
			res.Ask = Ask{ID: id, Mission: domain.MissionID(mission), State: "answered", Answer: answer, AnswerSrc: source}
			if !res.Replayed {
				res.Ask.AnsweredBy = by
			}
			return res, &WakePendingError{Ask: id, Mission: mission, Cause: err}
		}
		res.Wake = &w
	}
	a, err := s.GetAsk(ctx, id)
	res.Ask = a
	return res, err
}

// WakePendingError says an answer is stored but its wake was not recorded
// (review F8). It deliberately does not unwrap to the cause: a wake refused
// or timed out after the answer committed is not a refusal of the answer.
// Repeating the same answer or RepairAskWakes completes the wake.
type WakePendingError struct {
	Ask, Mission string
	Cause        error
}

func (e *WakePendingError) Error() string {
	return fmt.Sprintf("ask %s: answer stored, but the wake of mission %s was not recorded (fab ask repair retries it): %v",
		e.Ask, e.Mission, e.Cause)
}

const defaultRepairWakeTimeout = 10 * time.Second

func (s *Store) wakeForAsk(ctx context.Context, id string, mission domain.MissionID) (Response, error) {
	return s.Execute(ctx, AskActor, domain.CommandID(AskWakeKey(id)),
		domain.Command{Kind: domain.CmdWake, Mission: mission, Token: AskToken(id)})
}

// RepairAskWakes fires the wake of every answered ask whose wake was never
// recorded (a crash between the answer and the wake). Bounded by limit. Each
// wake has its own deadline and a failure does not stop the rest (review
// F8); it returns the wakes fired and every failure, joined.
func (s *Store) RepairAskWakes(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id, a.mission_id FROM asks a
		WHERE a.state = 'answered' AND a.mission_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM commands c WHERE c.actor = $1 AND c.mission_id = a.mission_id
		                  AND c.command_id = 'ask:' || a.id)
		ORDER BY a.answered_at, a.id LIMIT $2`, AskActor, limit)
	if err != nil {
		return 0, err
	}
	type pair struct{ id, mission string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.mission); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	timeout := s.repairWakeTimeout
	if timeout <= 0 {
		timeout = defaultRepairWakeTimeout
	}
	n := 0
	var failed []error
	for _, p := range todo {
		wctx, cancel := context.WithTimeout(ctx, timeout)
		_, err := s.wakeForAsk(wctx, p.id, domain.MissionID(p.mission))
		cancel()
		if err != nil {
			failed = append(failed, fmt.Errorf("ask %s: %w", p.id, err))
			if ctx.Err() != nil {
				break // the caller gave up; the rest would fail the same way
			}
			continue
		}
		n++
	}
	return n, errors.Join(failed...)
}

// WithdrawAsk closes an open ask without an answer. Withdrawing twice is a
// no-op; an answered ask cannot be withdrawn. A withdrawal fires no wake: a
// mission waiting on it stays waiting until someone decides (R13: nothing
// expires or moves on by itself).
func (s *Store) WithdrawAsk(ctx context.Context, id, reason string) error {
	if !askID.MatchString(id) {
		return fmt.Errorf("%w: ask id", domain.ErrInvalidID)
	}
	if reason == "" || len(reason) > 1024 {
		return fmt.Errorf("%w: a withdrawal needs a reason of 1..1024 bytes", domain.ErrInvalidTransition)
	}
	// Review F2: lock the row and decide on what the lock shows, as AnswerAsk
	// does. A single UPDATE ... RETURNING with a fallback SELECT reads the
	// fallback from the statement snapshot, so a withdrawal that waited on an
	// answer in flight saw "open" and reported success.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	var state string
	err = tx.QueryRow(ctx, "SELECT state FROM asks WHERE id = $1 FOR UPDATE", id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: ask %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	switch state {
	case "withdrawn":
		return nil // already withdrawn: idempotent
	case "open":
		tag, err := tx.Exec(ctx, "UPDATE asks SET state = 'withdrawn', withdrawn_reason = $2 WHERE id = $1 AND state = 'open'", id, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("ask %s: withdrawal updated %d rows", id, tag.RowsAffected())
		}
		return tx.Commit(ctx)
	default:
		return fmt.Errorf("%w: ask %s is already %s", domain.ErrInvalidTransition, id, state)
	}
}

// GetAsk returns one ask.
func (s *Store) GetAsk(ctx context.Context, id string) (Ask, error) {
	asks, err := s.listAsks(ctx, "", id)
	if err != nil {
		return Ask{}, err
	}
	if len(asks) == 0 {
		return Ask{}, fmt.Errorf("%w: ask %s", domain.ErrNotFound, id)
	}
	return asks[0], nil
}
