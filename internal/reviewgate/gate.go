package reviewgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	secretpolicy "fabric/internal/policy"
)

type Config struct {
	Policy          Policy
	Mode            RolloutMode
	Lease           time.Duration
	DenialThreshold int
	StateReader     StateReader
	Redactor        Redactor
	ResultLimit     int
}

type Gate struct {
	store        *PostgresStore
	reviewer     ModelReviewer
	executor     Executor
	stateReader  StateReader
	redactor     Redactor
	policy       Policy
	policyDigest string
	mode         RolloutMode
	lease        time.Duration
	threshold    int
	resultLimit  int
}

func NewGate(store *PostgresStore, reviewer ModelReviewer, executor Executor, cfg Config) (*Gate, error) {
	if store == nil || reviewer == nil {
		return nil, invalid("store and isolated reviewer are required")
	}
	cfg.Policy = clonePolicy(cfg.Policy) // callers cannot mutate policy after construction
	digest, err := PolicyDigest(cfg.Policy)
	if err != nil {
		return nil, err
	}
	if !validMode(cfg.Mode) {
		return nil, invalid("unknown rollout mode %q", cfg.Mode)
	}
	if cfg.Mode == Enforce && (executor == nil || cfg.StateReader == nil) {
		return nil, invalid("enforce mode requires an executor and trusted state reader")
	}
	if cfg.Lease == 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.Lease < 3*time.Millisecond || cfg.Lease > time.Hour {
		return nil, invalid("dispatch lease outside [3ms, 1h]")
	}
	if cfg.DenialThreshold == 0 {
		cfg.DenialThreshold = 3
	}
	if cfg.DenialThreshold < 1 || cfg.DenialThreshold > 1000 {
		return nil, invalid("denial threshold outside [1, 1000]")
	}
	if cfg.Redactor == nil {
		cfg.Redactor = secretpolicy.Redactor{}
	}
	if cfg.ResultLimit == 0 {
		cfg.ResultLimit = MaxDurableResultBytes
	}
	if cfg.ResultLimit < 128 || cfg.ResultLimit > MaxDurableResultBytes {
		return nil, invalid("durable result limit outside [128, 65536]")
	}
	return &Gate{store: store, reviewer: reviewer, executor: executor, stateReader: cfg.StateReader, redactor: cfg.Redactor,
		policy: cfg.Policy, policyDigest: digest, mode: cfg.Mode, lease: cfg.Lease, threshold: cfg.DenialThreshold, resultLimit: cfg.ResultLimit}, nil
}

// freshProposal replaces, rather than trusts, the caller's state claim.
func (g *Gate) freshProposal(ctx context.Context, p Proposal) (Proposal, error) {
	if g.stateReader == nil {
		return p, nil
	}
	fingerprint, err := g.stateReader.CurrentFingerprint(ctx, p.MissionID, p.SessionID)
	if err != nil {
		return Proposal{}, fmt.Errorf("trusted state reader: %w", err)
	}
	if !validText(fingerprint, MaxNameBytes) {
		return Proposal{}, invalid("trusted state reader returned an invalid fingerprint")
	}
	p.StateFingerprint = fingerprint
	return p, nil
}

func (g *Gate) Submit(ctx context.Context, proposerID string, p Proposal) (Decision, error) {
	if !validText(proposerID, MaxNameBytes) {
		return Decision{}, invalid("invalid proposer identity")
	}
	p, err := g.freshProposal(ctx, p)
	if err != nil {
		return Decision{}, err
	}
	canonical, err := CanonicalProposal(p)
	if err != nil {
		return Decision{}, err
	}
	hash := hashBytes(canonical)
	r, replayed, err := g.store.createRequestMode(ctx, proposerID, canonical, hash, p,
		g.policy.Version, g.policyDigest, g.mode, g.threshold)
	if err != nil {
		return Decision{}, err
	}
	d := decisionFor(r)
	d.Replayed = replayed
	return d, nil
}

// Inspect returns durable state without invoking a reviewer or executor.
func (g *Gate) Inspect(ctx context.Context, requestHash string) (Decision, error) {
	r, err := loadStored(ctx, g.store.pool, requestHash, false)
	if err != nil {
		return Decision{}, err
	}
	return decisionFor(r), nil
}

// Evaluate calls the reviewer outside database transactions and row locks. The
// final decision commits only if the exact request is still PENDING and fresh.
func (g *Gate) Evaluate(ctx context.Context, requestHash, originalTask string) (Decision, error) {
	if !validText(originalTask, MaxTaskBytes) {
		return Decision{}, invalid("original task is empty, too long or not UTF-8")
	}
	r, err := loadStored(ctx, g.store.pool, requestHash, false)
	if err != nil {
		return Decision{}, err
	}
	if r.PolicyVersion != g.policy.Version || r.PolicyDigest != g.policyDigest || r.Mode != g.mode {
		return Decision{}, fmt.Errorf("%w: policy content or rollout mode changed", ErrStale)
	}
	p, _, err := decodeStoredProposal(r)
	if err != nil {
		return Decision{}, err
	}
	fresh, err := g.freshProposal(ctx, p)
	if err != nil {
		return Decision{}, err
	}
	if fresh.StateFingerprint != p.StateFingerprint {
		return Decision{}, fmt.Errorf("%w: world state changed before evaluation", ErrStale)
	}
	if r.State != StatePending {
		d := decisionFor(r)
		d.Replayed = true
		return d, nil
	}
	circuitOpen, err := circuitIsOpen(ctx, g.store.pool, r.SessionID)
	if err != nil {
		return Decision{}, err
	}
	if circuitOpen {
		return Decision{}, ErrCircuitOpen
	}
	canonical := []byte(r.Canonical)
	risk, classifyErr := Classify(p, g.policy)
	var model ModelDecision
	var reviewErr error
	if classifyErr == nil {
		// This is the only reviewer call site and the only input it receives.
		model, reviewErr = callReviewer(ctx, g.reviewer, ReviewInput{
			OriginalTask: originalTask, CanonicalProposal: canonical, Risk: risk,
		})
	}
	// The reviewer may have run for an arbitrary caller-bounded duration. Read
	// trusted state again before recording the decision.
	fresh, err = g.freshProposal(ctx, p)
	if err != nil {
		return Decision{}, err
	}
	if fresh.StateFingerprint != p.StateFingerprint {
		return Decision{}, fmt.Errorf("%w: world state changed during review", ErrStale)
	}

	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err = loadStored(ctx, tx, requestHash, true)
	if err != nil {
		return Decision{}, err
	}
	if r.PolicyDigest != g.policyDigest || r.Canonical != string(canonical) {
		return Decision{}, fmt.Errorf("%w: request or policy changed during review", ErrStale)
	}
	if r.State != StatePending {
		d := decisionFor(r)
		d.Replayed = true
		return d, tx.Commit(ctx)
	}
	open, err := lockCircuit(ctx, tx, r.SessionID, g.threshold)
	if err != nil {
		return Decision{}, err
	}
	if open {
		return Decision{}, ErrCircuitOpen
	}

	denyReason := ""
	next := StateReady
	switch {
	case classifyErr != nil:
		if errors.Is(classifyErr, ErrDenied) {
			denyReason = "policy_default_deny"
		} else {
			denyReason = "invalid_policy_or_proposal"
		}
	case reviewErr != nil:
		denyReason = "reviewer_error"
	case model.Verdict == ReviewDeny:
		denyReason = "model_denied"
	case model.Verdict != ReviewAllow && model.Verdict != ReviewEscalate:
		denyReason = "malformed_model_decision"
	case risk.HumanRequired || model.Verdict == ReviewEscalate:
		// An allow above the model tier, or for irreversible/tainted effects,
		// lands here. There is no model-controlled risk downgrade channel.
		next = StateAwaitingHuman
	}
	if denyReason != "" {
		if err := denyLocked(ctx, tx, r, g.threshold, denyReason); err != nil {
			return Decision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Decision{}, err
		}
		d := decisionFor(r)
		d.State, d.Risk, d.Reason = StateDenied, risk, denyReason
		return d, fmt.Errorf("%w: %s", ErrDenied, denyReason)
	}
	tier := int(risk.Tier)
	if err := setEvaluated(ctx, tx, r, next, risk); err != nil {
		return Decision{}, err
	}
	event, actor := "MODEL_ALLOWED", r.ProposerID
	if next == StateAwaitingHuman {
		event = "HUMAN_REQUIRED"
	}
	if err := appendAudit(ctx, tx, r.Hash, event, actor, string(model.Verdict), r.PolicyVersion); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	r.State = next
	r.RiskTier, r.Reversibility, r.Tainted, r.HumanRequired = &tier, &risk.Reversibility, &risk.Tainted, &risk.HumanRequired
	d := decisionFor(r)
	if next == StateAwaitingHuman {
		d.Reason = "deterministic policy or model escalation requires a separate human operator"
	}
	return d, nil
}

func callReviewer(ctx context.Context, reviewer ModelReviewer, in ReviewInput) (decision ModelDecision, err error) {
	defer func() {
		if recover() != nil {
			decision = ModelDecision{}
			err = errors.New("reviewer panic")
		}
	}()
	return reviewer.Review(ctx, in)
}

func decodeStoredProposal(r storedRequest) (Proposal, []byte, error) {
	var p Proposal
	if err := json.Unmarshal([]byte(r.Canonical), &p); err != nil {
		return Proposal{}, nil, fmt.Errorf("%w: stored canonical proposal: %v", ErrInvalid, err)
	}
	canonical, err := CanonicalProposal(p)
	if err != nil {
		return Proposal{}, nil, err
	}
	if string(canonical) != r.Canonical || hashBytes(canonical) != r.Hash {
		return Proposal{}, nil, fmt.Errorf("%w: stored canonical proposal does not match its hash", ErrConflict)
	}
	return p, canonical, nil
}

func (g *Gate) Approve(ctx context.Context, approval Approval) (Decision, error) {
	if !validText(approval.OperatorID, MaxNameBytes) {
		return Decision{}, invalid("invalid operator identity")
	}
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, approval.RequestHash, true)
	if err != nil {
		return Decision{}, err
	}
	if approval.OperatorID == r.ProposerID {
		return Decision{}, fmt.Errorf("%w: operator must differ from proposer", ErrDenied)
	}
	var liveOperator string
	err = tx.QueryRow(ctx, `SELECT operator_id FROM review_gate_approvals
		WHERE request_hash = $1 AND policy_digest = $2 AND expires_at > clock_timestamp()
		ORDER BY expires_at DESC, id DESC LIMIT 1`, r.Hash, r.PolicyDigest).Scan(&liveOperator)
	if err == nil {
		if r.State != StateApproved {
			return Decision{}, fmt.Errorf("%w: live approval exists while request is %s", ErrConflict, r.State)
		}
		if liveOperator != approval.OperatorID {
			return Decision{}, fmt.Errorf("%w: approval belongs to a different operator", ErrConflict)
		}
		d := decisionFor(r)
		d.Replayed = true
		return d, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, err
	}
	// No live approval exists. APPROVED with an expired approval must renew,
	// never replay as live success.
	if r.State != StateAwaitingHuman && r.State != StateApproved {
		return Decision{}, fmt.Errorf("%w: request is %s", ErrApprovalRequired, r.State)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return Decision{}, err
	}
	if !approval.ExpiresAt.After(now) {
		return Decision{}, fmt.Errorf("%w: new approval is already expired", ErrDenied)
	}
	renewal := r.State == StateApproved
	if renewal {
		if _, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = 'AWAITING_HUMAN', updated_at = clock_timestamp()
			WHERE request_hash = $1 AND state = 'APPROVED'`, r.Hash); err != nil {
			return Decision{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO review_gate_approvals
		(request_hash, operator_id, proposer_id, policy_digest, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		r.Hash, approval.OperatorID, r.ProposerID, r.PolicyDigest, approval.ExpiresAt); err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = 'APPROVED', updated_at = clock_timestamp()
		WHERE request_hash = $1 AND state = 'AWAITING_HUMAN'`, r.Hash); err != nil {
		return Decision{}, err
	}
	detail := ""
	if renewal {
		detail = "renewal_after_expiry"
	}
	if approval.Reason != "" {
		if detail != "" {
			detail += ";"
		}
		detail += "reason_sha256=" + hashBytes([]byte(approval.Reason))
	}
	if err := appendAudit(ctx, tx, r.Hash, "HUMAN_APPROVED", approval.OperatorID, detail, r.PolicyVersion); err != nil {
		return Decision{}, err
	}
	if err := notifyReviewWake(ctx, tx, r.Hash); err != nil {
		return Decision{}, fmt.Errorf("review approval wake: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	r.State = StateApproved
	return decisionFor(r), nil
}

// Reject is a durable human denial with a separate authenticated actor.
func (g *Gate) Reject(ctx context.Context, rejection Rejection) (Decision, error) {
	if !validText(rejection.Actor, MaxNameBytes) {
		return Decision{}, invalid("invalid rejecting operator")
	}
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, rejection.RequestHash, true)
	if err != nil {
		return Decision{}, err
	}
	if rejection.Actor == r.ProposerID {
		return Decision{}, fmt.Errorf("%w: rejecting operator must differ from proposer", ErrDenied)
	}
	if r.State != StateAwaitingHuman && r.State != StateApproved {
		return Decision{}, fmt.Errorf("%w: cannot reject request in %s", ErrConflict, r.State)
	}
	detail := "human_rejected"
	if rejection.Reason != "" {
		detail += ";reason_sha256=" + hashBytes([]byte(rejection.Reason))
	}
	if err := recordDenialLocked(ctx, tx, r, g.threshold, "HUMAN_REJECTED", rejection.Actor, detail); err != nil {
		return Decision{}, err
	}
	if err := notifyReviewWake(ctx, tx, r.Hash); err != nil {
		return Decision{}, fmt.Errorf("review rejection wake: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	d := decisionFor(r)
	d.State = StateDenied
	d.Reason = "human_rejected"
	return d, nil
}

// Deny records a trusted outer-guard refusal for an existing, undispatched
// request. It cannot overwrite an active dispatch or any terminal outcome.
func (g *Gate) Deny(ctx context.Context, requestHash, reason string) (Decision, error) {
	if !validHash(requestHash) || !validText(reason, MaxAuditText) {
		return Decision{}, invalid("invalid review denial")
	}
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, requestHash, true)
	if err != nil {
		return Decision{}, err
	}
	if r.State == StateDenied {
		return decisionFor(r), tx.Commit(ctx)
	}
	if r.State != StatePending && r.State != StateReady && r.State != StateAwaitingHuman && r.State != StateApproved {
		return Decision{}, fmt.Errorf("%w: cannot deny request in %s", ErrConflict, r.State)
	}
	if err := denyLocked(ctx, tx, r, g.threshold, reason); err != nil {
		return Decision{}, err
	}
	if err := notifyReviewWake(ctx, tx, r.Hash); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	r.State = StateDenied
	return decisionFor(r), nil
}

// Dispatch freshly reads world state and reclassifies the complete current
// policy before revalidating the exact request under its row lock.
func (g *Gate) Dispatch(ctx context.Context, approvedHash string, current Proposal, idempotencyKey string) (DispatchResult, error) {
	if !validText(idempotencyKey, MaxNameBytes) {
		return DispatchResult{}, invalid("invalid dispatch idempotency key")
	}
	current, err := g.freshProposal(ctx, current)
	if err != nil {
		return DispatchResult{}, err
	}
	canonical, err := CanonicalProposal(current)
	if err != nil {
		return DispatchResult{}, err
	}
	if hashBytes(canonical) != approvedHash {
		return DispatchResult{}, fmt.Errorf("%w: current request hash or fresh state differs from approved hash", ErrStale)
	}
	initial, err := loadStored(ctx, g.store.pool, approvedHash, false)
	if err != nil {
		return DispatchResult{}, err
	}
	if initial.PolicyVersion != g.policy.Version || initial.Mode != g.mode {
		return DispatchResult{}, fmt.Errorf("%w: policy version or rollout mode changed", ErrStale)
	}
	// Re-evaluate even when the digest differs: a newly denied tool is refused
	// by current Classify, and every other content change is refused below.
	currentRisk, err := Classify(current, g.policy)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("%w: current policy rejects dispatch", err)
	}
	if initial.PolicyDigest != g.policyDigest {
		return DispatchResult{}, fmt.Errorf("%w: policy content digest changed", ErrStale)
	}
	if currentRisk != decisionFor(initial).Risk {
		return DispatchResult{}, fmt.Errorf("%w: current risk differs from evaluated risk", ErrStale)
	}

	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DispatchResult{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, approvedHash, true)
	if err != nil {
		return DispatchResult{}, err
	}
	if r.Canonical != string(canonical) || r.PolicyDigest != g.policyDigest || currentRisk != decisionFor(r).Risk {
		return DispatchResult{}, fmt.Errorf("%w: current bytes, state, policy or risk changed", ErrStale)
	}
	storedProposal, _, err := decodeStoredProposal(r)
	if err != nil {
		return DispatchResult{}, err
	}
	if storedProposal.StateFingerprint != current.StateFingerprint {
		return DispatchResult{}, fmt.Errorf("%w: state fingerprint changed", ErrStale)
	}

	if r.State == StateDispatched || r.State == StateUnknown || r.State == StateConfirmed || r.State == StateFailedDefinite {
		if r.DispatchKey == nil || *r.DispatchKey != idempotencyKey {
			return DispatchResult{}, fmt.Errorf("%w: request already dispatched under another key", ErrConflict)
		}
		outcome := Outcome(r.State)
		d := decisionFor(r)
		d.Replayed = true
		if r.State == StateUnknown {
			return DispatchResult{Decision: d, Outcome: Unknown}, ErrUnknown
		}
		return DispatchResult{Decision: d, Outcome: outcome}, tx.Commit(ctx)
	}
	if r.State != StateReady && r.State != StateApproved {
		return DispatchResult{}, fmt.Errorf("%w: request is %s", ErrApprovalRequired, r.State)
	}
	open, err := lockCircuit(ctx, tx, r.SessionID, g.threshold)
	if err != nil {
		return DispatchResult{}, err
	}
	if open {
		return DispatchResult{}, ErrCircuitOpen
	}
	if r.State == StateApproved {
		live, approvalErr := liveApproval(ctx, tx, r.Hash, g.policyDigest)
		if approvalErr != nil {
			return DispatchResult{}, fmt.Errorf("%w: live policy-bound approval missing", ErrApprovalRequired)
		}
		if !live {
			return DispatchResult{}, fmt.Errorf("%w: approval expired or invalid", ErrStale)
		}
	}
	if g.mode == Shadow || g.mode == Evaluate {
		if r.State == StateApproved {
			// A human-approved no-execution request has no dispatch. Move it to
			// READY, the legal state for exact reviewed-tool control settlement.
			if _, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = 'READY', updated_at = clock_timestamp()
				WHERE request_hash = $1 AND state = 'APPROVED' AND mode IN ('shadow','evaluate')`, r.Hash); err != nil {
				return DispatchResult{}, err
			}
			r.State = StateReady
		}
		event := "SHADOW_NO_EXECUTION"
		if g.mode == Evaluate {
			event = "EVALUATE_NO_EXECUTION"
		}
		if err := appendAudit(ctx, tx, r.Hash, event, r.ProposerID, "", r.PolicyVersion); err != nil {
			return DispatchResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return DispatchResult{}, err
		}
		d := decisionFor(r)
		d.Reason = event
		return DispatchResult{Decision: d}, nil
	}
	fence := r.Fence + 1
	var leaseUntil time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&leaseUntil); err != nil {
		return DispatchResult{}, err
	}
	leaseUntil = leaseUntil.Add(g.lease)
	if err := dispatchRequest(ctx, tx, r.Hash, idempotencyKey, fence, leaseUntil); err != nil {
		if isUniqueViolation(err) {
			return DispatchResult{}, fmt.Errorf("%w: idempotency key belongs to another request", ErrConflict)
		}
		return DispatchResult{}, err
	}
	if err := appendAudit(ctx, tx, r.Hash, "DISPATCHED", r.ProposerID, fmt.Sprintf("fence=%d", fence), r.PolicyVersion); err != nil {
		return DispatchResult{}, err
	}
	request := DispatchRequest{RequestHash: r.Hash, CanonicalProposal: canonical, IdempotencyKey: idempotencyKey, Fence: fence}
	execCtx, cancel := context.WithCancel(ctx)
	owner, err := g.store.registerOwner(r.Hash, idempotencyKey, fence, cancel)
	if err != nil {
		cancel()
		return DispatchResult{}, err
	}
	// Register before the row becomes visible. Once recovery can see this
	// DISPATCHED commit it also sees the active in-process owner.
	if err := tx.Commit(ctx); err != nil {
		cancel()
		g.store.unregisterOwner(r.Hash, owner)
		return DispatchResult{}, err
	}

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(g.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-execCtx.Done():
				return
			case <-ticker.C:
				if err := g.store.heartbeat(execCtx, r.Hash, idempotencyKey, fence, g.lease); err != nil {
					owner.cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-heartbeatDone
		g.store.unregisterOwner(r.Hash, owner)
	}()

	// Close the row-lock TOCTOU: this trusted read happens after the DISPATCHED
	// critical section and immediately before executor invocation. A stale call
	// settles FAILED_DEFINITE without executing. External state cannot be made
	// atomic with the executor after this final check.
	final, stateErr := g.freshProposal(ctx, current)
	if stateErr == nil && final.StateFingerprint != current.StateFingerprint {
		stateErr = fmt.Errorf("%w: world state changed after dispatch critical section", ErrStale)
	}
	if stateErr != nil {
		return g.failBeforeExecution(ctx, r.Hash, idempotencyKey, fence, stateErr)
	}

	// Heartbeat and active-owner registration cover the entire executor call and
	// its settlement. A panic leaves DISPATCHED for later recovery => UNKNOWN.
	result, execErr := g.executor.Execute(execCtx, request)
	outcome := Unknown
	var durable *pendingToolResult
	if execErr == nil && (result.Outcome == Confirmed || result.Outcome == FailedDefinite) {
		outcome = result.Outcome
		durable = g.prepareToolResult(result.Result)
	}
	return g.finishExecutionResult(ctx, r.Hash, idempotencyKey, fence, outcome, durable)
}

// failBeforeExecution records a definite non-success: the dispatch intent was
// committed, but the final trusted-state check proved execution must not start.
func (g *Gate) failBeforeExecution(ctx context.Context, hash, key string, fence int64, cause error) (DispatchResult, error) {
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("%w; no-execution settlement failed: %v", cause, err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, hash, true)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("%w; no-execution settlement failed: %v", cause, err)
	}
	if r.DispatchKey == nil || *r.DispatchKey != key || r.Fence != fence {
		return DispatchResult{}, errors.Join(cause, ErrStale)
	}
	if r.State == StateUnknown {
		return DispatchResult{Decision: decisionFor(r), Outcome: Unknown}, errors.Join(cause, ErrUnknown)
	}
	if r.State != StateDispatched {
		return DispatchResult{}, errors.Join(cause, ErrConflict)
	}
	if err := transitionRequest(ctx, tx, hash, StateDispatched, StateFailedDefinite); err != nil {
		return DispatchResult{}, fmt.Errorf("%w; no-execution settlement failed: %v", cause, err)
	}
	if err := appendAudit(ctx, tx, hash, "STALE_NO_EXECUTION", "state_reader", "", r.PolicyVersion); err != nil {
		return DispatchResult{}, fmt.Errorf("%w; no-execution audit failed: %v", cause, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DispatchResult{}, fmt.Errorf("%w; no-execution commit failed: %v", cause, err)
	}
	r.State = StateFailedDefinite
	return DispatchResult{Decision: decisionFor(r), Outcome: FailedDefinite}, cause
}

func (g *Gate) finishExecution(ctx context.Context, hash, key string, fence int64, outcome Outcome) (DispatchResult, error) {
	return g.finishExecutionResult(ctx, hash, key, fence, outcome, nil)
}

func (g *Gate) finishExecutionResult(ctx context.Context, hash, key string, fence int64, outcome Outcome, durable *pendingToolResult) (DispatchResult, error) {
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DispatchResult{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, hash, true)
	if err != nil {
		return DispatchResult{}, err
	}
	if r.DispatchKey == nil || *r.DispatchKey != key || r.Fence != fence {
		return DispatchResult{}, fmt.Errorf("%w: dispatch fence or key", ErrStale)
	}
	if r.State == StateUnknown {
		// A late executor result cannot overwrite recovery's conservative state.
		return DispatchResult{Decision: decisionFor(r), Outcome: Unknown, Owner: true}, ErrUnknown
	}
	if r.State != StateDispatched {
		return DispatchResult{}, fmt.Errorf("%w: execution finished in state %s", ErrConflict, r.State)
	}
	if err := transitionRequest(ctx, tx, hash, StateDispatched, RequestState(outcome)); err != nil {
		return DispatchResult{}, err
	}
	if outcome == Confirmed || outcome == FailedDefinite {
		if err := insertToolResult(ctx, tx, r, outcome, durable); err != nil {
			return DispatchResult{}, fmt.Errorf("durable redacted result: %w", err)
		}
	}
	event := "EXECUTION_UNKNOWN"
	if outcome == Confirmed {
		event = "EXECUTION_CONFIRMED"
	} else if outcome == FailedDefinite {
		event = "EXECUTION_FAILED_DEFINITE"
	}
	if err := appendAudit(ctx, tx, hash, event, "executor", "", r.PolicyVersion); err != nil {
		return DispatchResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DispatchResult{}, err
	}
	r.State = RequestState(outcome)
	result := DispatchResult{Decision: decisionFor(r), Outcome: outcome, Owner: true}
	if outcome == Unknown {
		return result, ErrUnknown
	}
	return result, nil
}

// Recover never settles an active owner using this store. Otherwise an expired
// crash-left DISPATCHED row becomes UNKNOWN; it is never automatically retried.
func (g *Gate) Recover(ctx context.Context, requestHash string) (Outcome, error) {
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, requestHash, true)
	if err != nil {
		return "", err
	}
	if r.State != StateDispatched {
		return Outcome(r.State), tx.Commit(ctx)
	}
	if g.store.ownerActive(requestHash) {
		return Dispatched, tx.Commit(ctx)
	}
	expired, err := dispatchExpired(ctx, tx, r.Hash)
	if err != nil {
		return "", err
	}
	if !expired {
		return Dispatched, tx.Commit(ctx)
	}
	if err := transitionRequest(ctx, tx, r.Hash, StateDispatched, StateUnknown); err != nil {
		return "", err
	}
	if err := appendAudit(ctx, tx, r.Hash, "RECOVERED_UNKNOWN", "recovery", "", r.PolicyVersion); err != nil {
		return "", err
	}
	return Unknown, tx.Commit(ctx)
}

// ListRecovery returns a bounded page of expired, non-active dispatch hashes.
func (g *Gate) ListRecovery(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, invalid("recovery page limit outside [1, 1000]")
	}
	rows, err := g.store.pool.Query(ctx, `SELECT request_hash FROM review_gate_requests
		WHERE state = 'DISPATCHED' AND lease_until <= clock_timestamp() ORDER BY lease_until, request_hash LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		if !g.store.ownerActive(hash) {
			out = append(out, hash)
		}
	}
	return out, rows.Err()
}

// Settle resolves UNKNOWN from an explicit readback or operator decision.
func (g *Gate) Settle(ctx context.Context, settlement Settlement) (Decision, error) {
	if settlement.Outcome != Confirmed && settlement.Outcome != FailedDefinite {
		return Decision{}, invalid("UNKNOWN settles only to CONFIRMED or FAILED_DEFINITE")
	}
	if !validText(settlement.Actor, MaxNameBytes) || !validText(settlement.Evidence, MaxAuditText) {
		return Decision{}, invalid("settlement actor and bounded evidence are required")
	}
	tx, err := g.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	r, err := loadStored(ctx, tx, settlement.RequestHash, true)
	if err != nil {
		return Decision{}, err
	}
	if r.State != StateUnknown || r.Fence != settlement.Fence {
		return Decision{}, fmt.Errorf("%w: settlement state or fence", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = $2, updated_at = clock_timestamp()
		WHERE request_hash = $1 AND state = 'UNKNOWN'`, r.Hash, RequestState(settlement.Outcome)); err != nil {
		return Decision{}, err
	}
	detail := "evidence_sha256=" + hashBytes([]byte(settlement.Evidence))
	if err := appendAudit(ctx, tx, r.Hash, "UNKNOWN_SETTLED", settlement.Actor, detail, r.PolicyVersion); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	r.State = RequestState(settlement.Outcome)
	return decisionFor(r), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
