package reviewgate

import (
	"context"
	"time"
)

// Operator is the separate human broker surface. It has no reviewer or
// executor and therefore cannot dispatch a tool or change its policy.
type Operator struct {
	store *PostgresStore
}

func NewOperator(store *PostgresStore) (*Operator, error) {
	if store == nil {
		return nil, invalid("operator review store is required")
	}
	return &Operator{store: store}, nil
}

func (o *Operator) gate() *Gate {
	return &Gate{store: o.store, threshold: 3}
}

func (o *Operator) Approve(ctx context.Context, approval Approval) (Decision, error) {
	return o.gate().Approve(ctx, approval)
}

func (o *Operator) Reject(ctx context.Context, rejection Rejection) (Decision, error) {
	return o.gate().Reject(ctx, rejection)
}

// RequestSummary excludes canonical arguments, original prompts, model output,
// dispatch keys, reasons, and evidence. It is safe for operator CLI display.
type RequestSummary struct {
	RequestHash  string         `json:"request_hash"`
	MissionID    string         `json:"mission_id"`
	SessionID    string         `json:"session_id"`
	Tool         string         `json:"tool"`
	ProposerID   string         `json:"proposer_id"`
	State        RequestState   `json:"state"`
	Mode         RolloutMode    `json:"mode"`
	Risk         RiskAssessment `json:"risk"`
	ApprovalEnds *time.Time     `json:"approval_expires_at,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

func summaryFor(r storedRequest) RequestSummary {
	d := decisionFor(r)
	return RequestSummary{RequestHash: r.Hash, MissionID: r.MissionID, SessionID: r.SessionID,
		Tool: r.Tool, ProposerID: r.ProposerID, State: r.State, Mode: r.Mode, Risk: d.Risk}
}

func (o *Operator) Inspect(ctx context.Context, requestHash string) (RequestSummary, error) {
	r, err := loadStored(ctx, o.store.pool, requestHash, false)
	if err != nil {
		return RequestSummary{}, err
	}
	s := summaryFor(r)
	if err := o.store.pool.QueryRow(ctx, `SELECT max(expires_at) FROM review_gate_approvals
		WHERE request_hash = $1 AND policy_digest = $2`, r.Hash, r.PolicyDigest).Scan(&s.ApprovalEnds); err != nil {
		return RequestSummary{}, err
	}
	if err := o.store.pool.QueryRow(ctx, `SELECT created_at, updated_at FROM review_gate_requests WHERE request_hash = $1`, r.Hash).
		Scan(&s.CreatedAt, &s.UpdatedAt); err != nil {
		return RequestSummary{}, err
	}
	return s, nil
}

func validRequestState(state RequestState) bool {
	switch state {
	case StatePending, StateDenied, StateReady, StateAwaitingHuman, StateApproved,
		StateDispatched, StateUnknown, StateConfirmed, StateFailedDefinite:
		return true
	}
	return false
}

// List returns metadata only, newest first. An empty state lists every state.
func (o *Operator) List(ctx context.Context, state RequestState, limit int) ([]RequestSummary, error) {
	if limit < 1 || limit > 1000 || (state != "" && !validRequestState(state)) {
		return nil, invalid("invalid review list state or page limit")
	}
	query := `SELECT request_hash, mission_id, session_id, tool, proposer_id, policy_version,
		policy_digest, mode, canonical_proposal, state, risk_tier, reversibility, tainted,
		human_required, dispatch_idem_key, dispatch_fence, lease_until, created_at, updated_at
		FROM review_gate_requests`
	args := []any{limit}
	if state != "" {
		query += ` WHERE state = $2`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC, request_hash LIMIT $1`
	rows, err := o.store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RequestSummary, 0, limit)
	for rows.Next() {
		var r storedRequest
		var s RequestSummary
		if err := rows.Scan(&r.Hash, &r.MissionID, &r.SessionID, &r.Tool, &r.ProposerID,
			&r.PolicyVersion, &r.PolicyDigest, &r.Mode, &r.Canonical, &r.State, &r.RiskTier,
			&r.Reversibility, &r.Tainted, &r.HumanRequired, &r.DispatchKey, &r.Fence,
			&r.LeaseUntil, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		summary := summaryFor(r)
		summary.CreatedAt, summary.UpdatedAt = s.CreatedAt, s.UpdatedAt
		out = append(out, summary)
	}
	return out, rows.Err()
}

// Recover exposes conservative recovery without granting operator dispatch.
func (o *Operator) Recover(ctx context.Context, requestHash string) (Outcome, error) {
	return o.gate().Recover(ctx, requestHash)
}
