package reviewgate

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Review authority uses direct app/owner SQL only. Runner connections never
// reach these helpers; their sole review-related operation is exact tool
// settlement in agent.DB.FinishReviewedTool.
func circuitIsOpen(ctx context.Context, pool *pgxpool.Pool, session string) (bool, error) {
	var open bool
	err := pool.QueryRow(ctx, `SELECT open FROM review_gate_circuits WHERE session_id = $1`, session).Scan(&open)
	if errorsIsNoRows(err) {
		return false, nil
	}
	return open, err
}

func errorsIsNoRows(err error) bool { return err == pgx.ErrNoRows }

func setEvaluated(ctx context.Context, tx pgx.Tx, r storedRequest, next RequestState, risk RiskAssessment) error {
	_, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = $2, risk_tier = $3,
		reversibility = $4, tainted = $5, human_required = $6, updated_at = clock_timestamp()
		WHERE request_hash = $1 AND state = 'PENDING'`,
		r.Hash, next, int(risk.Tier), risk.Reversibility, risk.Tainted, risk.HumanRequired)
	return err
}

func liveApproval(ctx context.Context, tx pgx.Tx, hash, digest string) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `SELECT expires_at > clock_timestamp() AND operator_id <> proposer_id
		FROM review_gate_approvals WHERE request_hash = $1 AND policy_digest = $2
		ORDER BY expires_at DESC, id DESC LIMIT 1`, hash, digest).Scan(&live)
	return live, err
}

func dispatchRequest(ctx context.Context, tx pgx.Tx, hash, key string, fence int64, leaseUntil time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = 'DISPATCHED', dispatch_idem_key = $2,
		dispatch_fence = $3, lease_until = $4, updated_at = clock_timestamp() WHERE request_hash = $1`,
		hash, key, fence, leaseUntil)
	return err
}

func transitionRequest(ctx context.Context, tx pgx.Tx, hash string, from, to RequestState) error {
	_, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = $2, lease_until = NULL, updated_at = clock_timestamp()
		WHERE request_hash = $1 AND state = $3`, hash, to, from)
	return err
}

func dispatchExpired(ctx context.Context, tx pgx.Tx, hash string) (bool, error) {
	var expired bool
	err := tx.QueryRow(ctx, `SELECT lease_until IS NULL OR lease_until <= clock_timestamp()
		FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&expired)
	return expired, err
}
