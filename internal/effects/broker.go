// Package effects is the fabric's effect broker (DESIGN.md 3.3): every
// external side effect is a row that moves INTENT -> DISPATCHED -> CONFIRMED |
// FAILED_DEFINITE | UNKNOWN, with UNKNOWN reconciled by readback and never
// replayed blindly. The business idempotency key is persisted before any
// transport and reused by every transport retry (plan invariant 8); the
// request hash is bound at submit and by any approval (AC-17); admission is
// rechecked in the transaction that records the dispatch (plan invariant 6).
package effects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
	"fabric/internal/store"
)

type State string

const (
	Intent           State = "INTENT"
	ApprovalRequired State = "APPROVAL_REQUIRED"
	Approved         State = "APPROVED"
	Dispatched       State = "DISPATCHED"
	Confirmed        State = "CONFIRMED"
	FailedDefinite   State = "FAILED_DEFINITE"
	Unknown          State = "UNKNOWN"
	Reconciling      State = "RECONCILING"
	NotApplied       State = "NOT_APPLIED"
	Quarantined      State = "QUARANTINED"
	// Review N2: an operator settled a QUARANTINED effect (ResolveQuarantined).
	ResolvedApplied    State = "RESOLVED_APPLIED"
	ResolvedNotApplied State = "RESOLVED_NOT_APPLIED"
)

var (
	ErrSubjectBusy      = errors.New("another effect on this subject is DISPATCHED or UNKNOWN")
	ErrApprovalRequired = errors.New("effect needs an approval bound to its request hash")
	ErrApprovalMismatch = errors.New("approval hash does not match the request hash")
	// ErrRequestTampered: the stored request no longer hashes to the stored request hash.
	ErrRequestTampered = errors.New("stored request does not match its hash")
	ErrNotDispatchable = errors.New("effect is not in a dispatchable state")
	ErrNotReconcilable = errors.New("effect is not UNKNOWN or RECONCILING")
	// ErrSettleRunEnded: the runner role may settle only a live run's
	// dispatch (migration 0019); the outcome stays DISPATCHED for readback.
	ErrSettleRunEnded = errors.New("dispatch outcome not recorded: run ended; reconcile with the app role")
)

// Intent is what a caller submits.
type IntentSpec struct {
	Mission       domain.MissionID
	Attempt       domain.AttemptID
	Fence         int64
	Kind          string
	Subject       string // e.g. ticket:goodqa#14791, branch:fabric/main
	IdemKey       string
	Request       json.RawMessage
	NeedsApproval bool
}

// Effect is the stored row.
type Effect struct {
	ID          string
	Mission     domain.MissionID
	Attempt     domain.AttemptID
	Fence       int64
	Kind        string
	Subject     string
	IdemKey     string
	Request     json.RawMessage
	RequestSHA  string
	State       State
	ApprovalSHA string
	Dispatches  int
	Receipt     json.RawMessage
	LastError   string
	Replayed    bool
}

// Submission is what an executor transports: the remote key is the
// business key, so the remote end can deduplicate.
type Submission struct {
	Effect    Effect
	RemoteKey string
}

// Outcome of a dispatch as the executor saw it. UNKNOWN carries no positive
// evidence (plan invariant 7).
type Outcome string

const (
	OutcomeConfirmed      Outcome = "CONFIRMED"
	OutcomeFailedDefinite Outcome = "FAILED_DEFINITE"
	OutcomeUnknown        Outcome = "UNKNOWN"
)

// Receipt is the executor's evidence for a dispatch.
type Receipt struct {
	Outcome Outcome         `json:"outcome"`
	Detail  string          `json:"detail,omitempty"`
	Remote  json.RawMessage `json:"remote,omitempty"` // e.g. the comment id returned
}

// Observation is the result of a token-free readback.
type Observation string

const (
	ObsCommitted  Observation = "COMMITTED"
	ObsNotApplied Observation = "NOT_APPLIED"
	ObsConflict   Observation = "CONFLICT"
	ObsUnknown    Observation = "UNKNOWN"
)

// Executor is the per-kind transport (Kimi's EffectExecutor, DESIGN.md 3.3).
type Executor interface {
	Kind() string
	Validate(spec IntentSpec) error
	Precondition(ctx context.Context, e Effect) (ok bool, detail string, err error)
	Dispatch(ctx context.Context, sub Submission) (Receipt, error)
	Reconcile(ctx context.Context, e Effect) (Observation, string, error)
}

// Broker owns the effects table through the store's pool.
type Broker struct {
	st   *store.Store
	pool *pgxpool.Pool
	// settleRefused counts dispatch outcomes the database refused to record
	// because the run had ended (ErrSettleRunEnded).
	settleRefused atomic.Int64

	// Crash cutpoints for tests: after the dispatch row commits but before
	// transport, and after transport but before the receipt commits.
	afterDispatchCommit func(Effect)
	afterTransport      func(Effect, Receipt)
}

func New(st *store.Store) *Broker { return &Broker{st: st, pool: st.Pool()} }

// effectID derives a stable row id from the business key.
func effectID(idem string) string {
	sum := sha256.Sum256([]byte(idem))
	return "e_" + hex.EncodeToString(sum[:])[:24]
}

// Submit records the intent under its key. Same key + same canonical request
// returns the stored row (Replayed); same key + different request is
// IDEMPOTENCY_CONFLICT. The fence must be the attempt's live fence.
func (b *Broker) Submit(ctx context.Context, exec Executor, spec IntentSpec) (Effect, error) {
	if spec.IdemKey == "" || spec.Subject == "" || spec.Kind == "" || spec.Kind != exec.Kind() {
		return Effect{}, fmt.Errorf("%w: intent needs kind=%q, subject and key", domain.ErrInvalidTransition, exec.Kind())
	}
	if err := exec.Validate(spec); err != nil {
		return Effect{}, err
	}
	base, err := domain.EffectBase(spec.Kind, spec.Request)
	if err != nil {
		return Effect{}, err
	}
	canon, _ := domain.CanonicalJSON(spec.Request)
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	// Replay first (Kimi effect-identity analysis): a settled effect replays
	// after its attempt ended, and the key binds the whole intent, not only
	// the request. Only a new intent needs a live fence.
	existing, err := b.loadTx(ctx, tx, "idem_key", spec.IdemKey)
	if err == nil {
		if existing.RequestSHA != base || existing.Mission != spec.Mission || existing.Attempt != spec.Attempt ||
			existing.Fence != spec.Fence || existing.Kind != spec.Kind || existing.Subject != spec.Subject {
			return Effect{}, fmt.Errorf("%w: effect key %s", domain.ErrIdempotencyConflict, spec.IdemKey)
		}
		if existing.neededApproval() != spec.NeedsApproval {
			return Effect{}, fmt.Errorf("%w: effect key %s was submitted with needs-approval %t, this replay says %t",
				domain.ErrIdempotencyConflict, spec.IdemKey, existing.neededApproval(), spec.NeedsApproval)
		}
		existing.Replayed = true
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, err
	}
	if err := liveFence(ctx, tx, spec.Mission, spec.Attempt, spec.Fence); err != nil {
		return Effect{}, err
	}
	state := Intent
	if spec.NeedsApproval {
		state = ApprovalRequired
	}
	id := effectID(spec.IdemKey)
	if _, err := tx.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, string(spec.Mission), string(spec.Attempt), spec.Fence, spec.Kind, spec.Subject, spec.IdemKey, canon, base, string(state)); err != nil {
		return Effect{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Effect{}, err
	}
	return b.Load(ctx, id)
}

// liveFence: the attempt is live at this fence and belongs to the effect's
// mission (review Q3 on 3f38cc3).
func liveFence(ctx context.Context, tx pgx.Tx, mission domain.MissionID, attempt domain.AttemptID, fence int64) error {
	var cur int64
	var state, owner string
	err := tx.QueryRow(ctx, "SELECT fence, state, mission_id FROM attempts WHERE id = $1", string(attempt)).Scan(&cur, &state, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return err
	}
	if owner != string(mission) {
		return fmt.Errorf("%w: attempt %s belongs to mission %s, not %s", domain.ErrInvalidTransition, attempt, owner, mission)
	}
	if cur != fence {
		return fmt.Errorf("%w: effect fence %d, attempt holds %d", domain.ErrStaleFence, fence, cur)
	}
	if !domain.AttemptState(state).Live() {
		return fmt.Errorf("%w: attempt %s is %s", domain.ErrInvalidTransition, attempt, state)
	}
	return nil
}

// neededApproval reports whether the effect was submitted as needing an
// approval: it is waiting for one, or one was recorded (red-team F6).
func (e Effect) neededApproval() bool {
	return e.State == ApprovalRequired || e.ApprovalSHA != ""
}

// Approve binds an approval to the exact request hash (AC-17): a changed
// byte needs a new approval.
func (b *Broker) Approve(ctx context.Context, id, requestSHA, by string) error {
	tag, err := b.pool.Exec(ctx, `UPDATE effects SET state = $2, approval_sha = $3, approved_by = $4, updated_at = now()
		WHERE id = $1 AND state = $5 AND request_sha = $3`, id, string(Approved), requestSHA, by, string(ApprovalRequired))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	e, err := b.Load(ctx, id)
	if err != nil {
		return err
	}
	if e.State != ApprovalRequired {
		return fmt.Errorf("%w: effect %s is %s", domain.ErrInvalidTransition, id, e.State)
	}
	return fmt.Errorf("%w: approval for %s cites %s, request is %s", ErrApprovalMismatch, id, requestSHA[:8], e.RequestSHA[:8])
}

// Dispatch admits the effect in one transaction (state, approval hash, live
// fence, one live effect per subject), commits DISPATCHED, then transports.
// A transport error or ambiguous result is UNKNOWN, never a retry.
func (b *Broker) Dispatch(ctx context.Context, exec Executor, id string) (Effect, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	e, err := b.loadTx(ctx, tx, "id", id)
	if err != nil {
		return Effect{}, err
	}
	if e.Kind != exec.Kind() {
		return Effect{}, fmt.Errorf("%w: executor %s for effect kind %s", domain.ErrInvalidTransition, exec.Kind(), e.Kind)
	}
	// What is sent is what was hashed and approved: recompute before transport.
	if sha, err := domain.EffectBase(e.Kind, e.Request); err != nil || sha != e.RequestSHA {
		return Effect{}, fmt.Errorf("%w: effect %s", ErrRequestTampered, id)
	}
	switch e.State {
	case Intent:
	case Approved:
		if e.ApprovalSHA != e.RequestSHA {
			return Effect{}, fmt.Errorf("%w: effect %s", ErrApprovalMismatch, id)
		}
	case ApprovalRequired:
		return Effect{}, fmt.Errorf("%w: effect %s", ErrApprovalRequired, id)
	default:
		return Effect{}, fmt.Errorf("%w: effect %s is %s", ErrNotDispatchable, id, e.State)
	}
	if err := liveFence(ctx, tx, e.Mission, e.Attempt, e.Fence); err != nil {
		return Effect{}, err
	}
	ok, detail, err := exec.Precondition(ctx, e)
	if err != nil {
		return Effect{}, err
	}
	// Review O3: Precondition may take a network round trip; the attempt
	// can end meanwhile. Re-check (the 0020 trigger holds it from here).
	if err := liveFence(ctx, tx, e.Mission, e.Attempt, e.Fence); err != nil {
		return Effect{}, err
	}
	if !ok {
		if _, err := tx.Exec(ctx, `UPDATE effects SET state = $2, last_error = $3, updated_at = now() WHERE id = $1`, id, string(FailedDefinite), "precondition: "+detail); err != nil {
			return Effect{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Effect{}, err
		}
		return b.Load(ctx, id)
	}
	if _, err := tx.Exec(ctx, `UPDATE effects SET state = $2, dispatches = dispatches + 1, updated_at = now() WHERE id = $1`, id, string(Dispatched)); err != nil {
		if store.IsUniqueViolation(err) {
			return Effect{}, fmt.Errorf("%w: %s", ErrSubjectBusy, e.Subject)
		}
		return Effect{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		if store.IsUniqueViolation(err) {
			return Effect{}, fmt.Errorf("%w: %s", ErrSubjectBusy, e.Subject)
		}
		return Effect{}, err
	}
	e.State = Dispatched
	e.Dispatches++
	if b.afterDispatchCommit != nil {
		b.afterDispatchCommit(e)
	}
	rc, terr := exec.Dispatch(ctx, Submission{Effect: e, RemoteKey: e.IdemKey})
	if terr != nil {
		rc = Receipt{Outcome: OutcomeUnknown, Detail: "transport: " + terr.Error()}
	}
	if b.afterTransport != nil {
		b.afterTransport(e, rc)
	}
	return b.settle(ctx, id, e.Dispatches, rc)
}

// settle records a transport receipt only on the dispatch it came from: a
// row that moved on to a later dispatch keeps its state.
func (b *Broker) settle(ctx context.Context, id string, dispatch int, rc Receipt) (Effect, error) {
	var state State
	switch rc.Outcome {
	case OutcomeConfirmed:
		state = Confirmed
	case OutcomeFailedDefinite:
		state = FailedDefinite
	default:
		state = Unknown
	}
	rcJSON, _ := json.Marshal(rc)
	if _, err := b.pool.Exec(ctx, `UPDATE effects SET state = $2, receipt = $3, last_error = $4, updated_at = now() WHERE id = $1 AND state = $5 AND dispatches = $6`,
		id, string(state), rcJSON, rc.Detail, string(Dispatched), dispatch); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42501" {
			b.settleRefused.Add(1)
			return Effect{}, fmt.Errorf("%w: effect %s (%s)", ErrSettleRunEnded, id, pe.Message)
		}
		return Effect{}, err
	}
	return b.Load(ctx, id)
}

// Reconcile is a token-free readback for an UNKNOWN effect (plan invariant
// 9). It never dispatches.
func (b *Broker) Reconcile(ctx context.Context, exec Executor, id string) (Effect, error) {
	tag, err := b.pool.Exec(ctx, `UPDATE effects SET state = $2, updated_at = now() WHERE id = $1 AND state IN ($3, $2)`,
		id, string(Reconciling), string(Unknown))
	if err != nil {
		return Effect{}, err
	}
	if tag.RowsAffected() != 1 {
		return Effect{}, fmt.Errorf("%w: effect %s", ErrNotReconcilable, id)
	}
	e, err := b.Load(ctx, id)
	if err != nil {
		return Effect{}, err
	}
	obs, detail, oerr := exec.Reconcile(ctx, e)
	if oerr != nil {
		obs, detail = ObsUnknown, "readback: "+oerr.Error()
	}
	var state State
	switch obs {
	case ObsCommitted:
		state = Confirmed
	case ObsNotApplied:
		state = NotApplied
	case ObsConflict:
		state = Quarantined
	default:
		state = Unknown // still unresolved; a later sweep tries again
	}
	rc, _ := json.Marshal(Receipt{Outcome: Outcome(obs), Detail: detail})
	if _, err := b.pool.Exec(ctx, `UPDATE effects SET state = $2, receipt = $3, last_error = $4, updated_at = now() WHERE id = $1 AND state = $5`,
		id, string(state), rc, detail, string(Reconciling)); err != nil {
		return Effect{}, err
	}
	return b.Load(ctx, id)
}

// Retry re-arms a FAILED_DEFINITE or NOT_APPLIED effect under the same key.
// UNKNOWN is refused: reconcile first.
func (b *Broker) Retry(ctx context.Context, id string) (Effect, error) {
	e, err := b.Load(ctx, id)
	if err != nil {
		return Effect{}, err
	}
	switch e.State {
	case Unknown, Reconciling:
		return Effect{}, fmt.Errorf("%w: effect %s", domain.ErrUnknownUnresolved, id)
	case FailedDefinite, NotApplied:
	default:
		return Effect{}, fmt.Errorf("%w: retry while %s", domain.ErrInvalidTransition, e.State)
	}
	next := Intent
	if e.ApprovalSHA != "" {
		next = Approved // the approval still binds the same request hash
	}
	if _, err := b.pool.Exec(ctx, `UPDATE effects SET state = $2, updated_at = now() WHERE id = $1 AND state = $3`, id, string(next), string(e.State)); err != nil {
		return Effect{}, err
	}
	return b.Load(ctx, id)
}

// Load reads one effect.
func (b *Broker) Load(ctx context.Context, id string) (Effect, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	return b.loadTx(ctx, tx, "id", id)
}

func (b *Broker) loadTx(ctx context.Context, tx pgx.Tx, col, val string) (Effect, error) {
	if col != "id" && col != "idem_key" {
		return Effect{}, errors.New("bad column")
	}
	var e Effect
	var approval *string
	err := tx.QueryRow(ctx, `SELECT id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state,
		approval_sha, dispatches, receipt, last_error FROM effects WHERE `+col+` = $1 FOR UPDATE`, val).Scan(
		&e.ID, &e.Mission, &e.Attempt, &e.Fence, &e.Kind, &e.Subject, &e.IdemKey, &e.Request, &e.RequestSHA, &e.State,
		&approval, &e.Dispatches, &e.Receipt, &e.LastError)
	if err != nil {
		return Effect{}, err
	}
	if approval != nil {
		e.ApprovalSHA = *approval
	}
	return e, nil
}

// Unresolved lists UNKNOWN/RECONCILING effects older than age, bounded.
func (b *Broker) Unresolved(ctx context.Context, age time.Duration, limit int) ([]string, error) {
	rows, err := b.pool.Query(ctx, `SELECT id FROM effects WHERE state IN ($1, $2) AND updated_at < now() - $3::interval ORDER BY updated_at LIMIT $4`,
		string(Unknown), string(Reconciling), fmt.Sprintf("%d milliseconds", age.Milliseconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SettleRefused is how many dispatch outcomes this broker could not record
// because their run had ended.
func (b *Broker) SettleRefused() int64 { return b.settleRefused.Load() }

// ResolveQuarantined is the operator exit for a QUARANTINED effect (review
// N2 on aecf3fa): an app-role actor who has established the outcome by
// other means (a manual readback, the remote's own log) records it with a
// reason and an evidence reference. The audit row and the state change
// commit together; migration 0020 refuses either without the other, and
// the runner role can write neither.
func (b *Broker) ResolveQuarantined(ctx context.Context, id string, outcome State, reason, evidence, actor string) (Effect, error) {
	if outcome != ResolvedApplied && outcome != ResolvedNotApplied {
		return Effect{}, fmt.Errorf("%w: a quarantined effect resolves to %s or %s, not %q", domain.ErrInvalidTransition, ResolvedApplied, ResolvedNotApplied, outcome)
	}
	if reason == "" || evidence == "" || actor == "" {
		return Effect{}, fmt.Errorf("%w: resolving a quarantined effect needs a reason, an evidence reference and an actor", domain.ErrInvalidTransition)
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	var state string
	err = tx.QueryRow(ctx, "SELECT state FROM effects WHERE id = $1 FOR UPDATE", id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, fmt.Errorf("%w: effect %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return Effect{}, err
	}
	if State(state) != Quarantined {
		return Effect{}, fmt.Errorf("%w: resolve while %s (needs QUARANTINED)", domain.ErrInvalidTransition, state)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, state, string(outcome), reason, evidence, actor); err != nil {
		return Effect{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE effects SET state = $2, last_error = $3, updated_at = now() WHERE id = $1`,
		id, string(outcome), "resolved by "+actor+": "+reason); err != nil {
		return Effect{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Effect{}, err
	}
	return b.Load(ctx, id)
}

// Abandon settles a never-dispatched intent whose attempt has ended (main
// ruling on the 717bb09 review nit): INTENT, APPROVAL_REQUIRED or APPROVED
// with dispatches = 0 can no longer be sent (no live fence) and never was,
// so it is NOT_APPLIED. An effect that was ever dispatched goes through
// readback or ResolveQuarantined instead. Audited like a resolution, in the
// same transaction (0020 refuses the move without the row). App role only.
func (b *Broker) Abandon(ctx context.Context, id, reason, evidence, actor string) (Effect, error) {
	if reason == "" || evidence == "" || actor == "" {
		return Effect{}, fmt.Errorf("%w: abandoning an effect needs a reason, an evidence reference and an actor", domain.ErrInvalidTransition)
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Effect{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	var state, attempt, mission string
	var fence int64
	var dispatches int
	err = tx.QueryRow(ctx, "SELECT state, attempt_id, mission_id, fence, dispatches FROM effects WHERE id = $1 FOR UPDATE", id).Scan(&state, &attempt, &mission, &fence, &dispatches)
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, fmt.Errorf("%w: effect %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return Effect{}, err
	}
	switch State(state) {
	case Intent, ApprovalRequired, Approved:
	default:
		return Effect{}, fmt.Errorf("%w: abandon while %s", domain.ErrInvalidTransition, state)
	}
	if dispatches != 0 {
		return Effect{}, fmt.Errorf("%w: effect %s was dispatched %d time(s); settle it by readback or fab effect resolve", domain.ErrInvalidTransition, id, dispatches)
	}
	if liveFence(ctx, tx, domain.MissionID(mission), domain.AttemptID(attempt), fence) == nil {
		return Effect{}, fmt.Errorf("%w: attempt %s can still dispatch effect %s", domain.ErrAttemptLive, attempt, id)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, state, string(NotApplied), reason, evidence, actor); err != nil {
		return Effect{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE effects SET state = $2, last_error = $3, updated_at = now() WHERE id = $1`,
		id, string(NotApplied), "abandoned by "+actor+": "+reason); err != nil {
		return Effect{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Effect{}, err
	}
	return b.Load(ctx, id)
}
