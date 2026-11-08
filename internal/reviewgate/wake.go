package reviewgate

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// reviewWakeChannel carries only a request hash. Delivery is a wake hint, not
// authority: the durable request row remains the sole decision checkpoint.
const reviewWakeChannel = "fabric_review_gate_wake"

// reviewWakePoll bounds recovery from a missed NOTIFY or a waiter that starts
// after the approval commit. It also bounds connection use to one LISTEN conn.
const reviewWakePoll = time.Second

func notifyReviewWake(ctx context.Context, tx pgx.Tx, requestHash string) error {
	_, err := tx.Exec(ctx, "SELECT pg_notify('"+reviewWakeChannel+"', $1)", requestHash)
	return err
}

func validHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 32
}

// WaitForReviewDecision blocks while the exact request awaits a human. LISTEN
// is installed before the first state read, closing the subscribe/check race;
// the bounded poll makes an already-committed or lost notification harmless.
// Approval never extends an attempt lease: the native caller heartbeats under
// its existing identity and fence for the entire wait.
func (s *PostgresStore) WaitForReviewDecision(ctx context.Context, requestHash string) error {
	if !validHash(requestHash) {
		return invalid("invalid review request hash")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+reviewWakeChannel); err != nil {
		return fmt.Errorf("review wake listen: %w", err)
	}
	for {
		var state RequestState
		var waiting bool
		err = conn.QueryRow(ctx, `SELECT r.state,
			CASE r.state
				WHEN 'AWAITING_HUMAN' THEN true
				WHEN 'APPROVED' THEN NOT EXISTS (
					SELECT 1 FROM review_gate_approvals a WHERE a.request_hash = r.request_hash
					  AND a.policy_digest = r.policy_digest AND a.operator_id <> r.proposer_id
					  AND a.expires_at > clock_timestamp())
				WHEN 'DISPATCHED' THEN r.lease_until IS NOT NULL AND r.lease_until > clock_timestamp()
				ELSE false
			END
			FROM review_gate_requests r WHERE r.request_hash = $1`, requestHash).Scan(&state, &waiting)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: request %s", ErrNotFound, requestHash)
		}
		if err != nil {
			return err
		}
		if !waiting {
			return nil
		}

		waitCtx, cancel := context.WithTimeout(ctx, reviewWakePoll)
		notification, waitErr := conn.Conn().WaitForNotification(waitCtx)
		waitCtxErr := waitCtx.Err()
		cancel()
		if waitErr == nil && notification != nil && notification.Payload == requestHash {
			continue // recheck the durable row before resuming any work
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if waitErr != nil && waitCtxErr == nil {
			// A broken connection cannot be allowed to spin or fabricate a wake.
			return fmt.Errorf("review wake wait: %w", waitErr)
		}
		// Timeout: poll the durable state. NOTIFY is deliberately not required.
	}
}
