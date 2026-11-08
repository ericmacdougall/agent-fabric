package reviewgate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore owns only review-gate access. Schema installation stays with
// the repository's store.Store.Migrate roll-forward migration runner.
type PostgresStore struct {
	pool *pgxpool.Pool

	// owners is per store, shared by every Gate using it. Recovery checks this
	// registry before expiring a row, so an active in-process call is never
	// settled behind its owner's back.
	mu     sync.Mutex
	owners map[string]*activeDispatch
}

type activeDispatch struct {
	key    string
	fence  int64
	cancel context.CancelFunc
	done   chan struct{}
}

func Open(ctx context.Context, dsn string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresStore{pool: pool, owners: map[string]*activeDispatch{}}, nil
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, owners: map[string]*activeDispatch{}}
}
func (s *PostgresStore) Close()              { s.pool.Close() }
func (s *PostgresStore) Pool() *pgxpool.Pool { return s.pool }

func (s *PostgresStore) registerOwner(hash, key string, fence int64, cancel context.CancelFunc) (*activeDispatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.owners[hash]; old != nil {
		select {
		case <-old.done:
		default:
			return nil, fmt.Errorf("%w: %s", ErrNotDispatchOwner, hash)
		}
	}
	o := &activeDispatch{key: key, fence: fence, cancel: cancel, done: make(chan struct{})}
	s.owners[hash] = o
	return o, nil
}

func (s *PostgresStore) unregisterOwner(hash string, owner *activeDispatch) {
	s.mu.Lock()
	if s.owners[hash] == owner {
		delete(s.owners, hash)
	}
	select {
	case <-owner.done:
	default:
		close(owner.done)
	}
	s.mu.Unlock()
}

func (s *PostgresStore) ownerActive(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.owners[hash]
	if o == nil {
		return false
	}
	select {
	case <-o.done:
		return false
	default:
		return true
	}
}

// heartbeat extends only a still-live, exactly matching DISPATCHED row. It can
// never resurrect an expired/UNKNOWN row or overwrite a terminal outcome.
func (s *PostgresStore) heartbeat(ctx context.Context, hash, key string, fence int64, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `UPDATE review_gate_requests
		SET lease_until = clock_timestamp() + $4 * interval '1 microsecond', updated_at = clock_timestamp()
		WHERE request_hash = $1 AND dispatch_idem_key = $2 AND dispatch_fence = $3
		  AND state = 'DISPATCHED' AND lease_until > clock_timestamp()`,
		hash, key, fence, lease.Microseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: heartbeat lost fresh dispatch ownership", ErrStale)
	}
	return nil
}

type storedRequest struct {
	Hash          string
	MissionID     string
	SessionID     string
	Tool          string
	ProposerID    string
	PolicyVersion string
	PolicyDigest  string
	Mode          RolloutMode
	Canonical     string
	State         RequestState
	RiskTier      *int
	Reversibility *Reversibility
	Tainted       *bool
	HumanRequired *bool
	DispatchKey   *string
	Fence         int64
	LeaseUntil    *time.Time
}

func scanStored(row interface{ Scan(...any) error }) (storedRequest, error) {
	var r storedRequest
	err := row.Scan(&r.Hash, &r.MissionID, &r.SessionID, &r.Tool, &r.ProposerID,
		&r.PolicyVersion, &r.PolicyDigest, &r.Mode, &r.Canonical, &r.State, &r.RiskTier,
		&r.Reversibility, &r.Tainted, &r.HumanRequired, &r.DispatchKey, &r.Fence, &r.LeaseUntil)
	return r, err
}

const storedColumns = `request_hash, mission_id, session_id, tool, proposer_id,
	policy_version, policy_digest, mode, canonical_proposal, state, risk_tier, reversibility,
	tainted, human_required, dispatch_idem_key, dispatch_fence, lease_until`

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadStored(ctx context.Context, q rowQuerier, hash string, lock bool) (storedRequest, error) {
	query := "SELECT " + storedColumns + " FROM review_gate_requests WHERE request_hash = $1"
	if lock {
		query += " FOR UPDATE"
	}
	r, err := scanStored(q.QueryRow(ctx, query, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return storedRequest{}, fmt.Errorf("%w: request %s", ErrNotFound, hash)
	}
	return r, err
}

func (s *PostgresStore) createRequestMode(ctx context.Context, proposer string, canonical []byte, hash string, p Proposal, policyVersion, policyDigest string, mode RolloutMode, threshold int) (storedRequest, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return storedRequest{}, false, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	open, err := lockCircuit(ctx, tx, p.SessionID, threshold)
	if err != nil {
		return storedRequest{}, false, err
	}
	if open {
		return storedRequest{}, false, ErrCircuitOpen
	}
	tag, err := tx.Exec(ctx, `INSERT INTO review_gate_requests
		(request_hash, mission_id, session_id, tool, proposer_id, policy_version, policy_digest, mode, canonical_proposal, state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'PENDING') ON CONFLICT (request_hash) DO NOTHING`,
		hash, p.MissionID, p.SessionID, p.Tool, proposer, policyVersion, policyDigest, mode, string(canonical))
	if err != nil {
		return storedRequest{}, false, err
	}
	inserted := tag.RowsAffected() == 1
	r, err := loadStored(ctx, tx, hash, false)
	if err != nil {
		return storedRequest{}, false, err
	}
	if inserted {
		if err := appendAudit(ctx, tx, hash, "SUBMITTED", proposer, "", policyVersion); err != nil {
			return storedRequest{}, false, err
		}
	} else if r.ProposerID != proposer || r.Canonical != string(canonical) || r.PolicyVersion != policyVersion || r.PolicyDigest != policyDigest || r.Mode != mode {
		return storedRequest{}, false, fmt.Errorf("%w: request hash already exists with different submission metadata", ErrConflict)
	}
	return r, !inserted, tx.Commit(ctx)
}

func decisionFor(r storedRequest) Decision {
	d := Decision{RequestHash: r.Hash, State: r.State, Mode: r.Mode,
		Risk: RiskAssessment{PolicyVersion: r.PolicyVersion, PolicyDigest: r.PolicyDigest}}
	if r.RiskTier != nil {
		d.Risk.Tier = RiskTier(*r.RiskTier)
	}
	if r.Reversibility != nil {
		d.Risk.Reversibility = *r.Reversibility
		d.Risk.SideEffect = *r.Reversibility != NoSideEffect
	}
	if r.Tainted != nil {
		d.Risk.Tainted = *r.Tainted
	}
	if r.HumanRequired != nil {
		d.Risk.HumanRequired = *r.HumanRequired
	}
	return d
}

func lockCircuit(ctx context.Context, tx pgx.Tx, session string, threshold int) (bool, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO review_gate_circuits (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, session); err != nil {
		return false, err
	}
	var open bool
	err := tx.QueryRow(ctx, `SELECT open FROM review_gate_circuits WHERE session_id = $1 FOR UPDATE`, session).Scan(&open)
	return open, err
}

func denyLocked(ctx context.Context, tx pgx.Tx, r storedRequest, threshold int, reason string) error {
	return recordDenialLocked(ctx, tx, r, threshold, "DENIED", r.ProposerID, reason)
}

// recordDenialLocked updates both the request and its session circuit. The
// caller holds the request lock and passes only a fixed non-secret audit code.
func recordDenialLocked(ctx context.Context, tx pgx.Tx, r storedRequest, threshold int, event, actor, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE review_gate_requests SET state = 'DENIED', updated_at = clock_timestamp()
		WHERE request_hash = $1 AND state IN ('PENDING', 'READY', 'AWAITING_HUMAN', 'APPROVED')`, r.Hash); err != nil {
		return err
	}
	var count int64
	if err := tx.QueryRow(ctx, `INSERT INTO review_gate_circuits (session_id, denial_count, open, updated_at)
		VALUES ($1, 1, $2::int <= 1, clock_timestamp())
		ON CONFLICT (session_id) DO UPDATE SET denial_count = review_gate_circuits.denial_count + 1,
			open = review_gate_circuits.open OR review_gate_circuits.denial_count + 1 >= $2::int,
			updated_at = clock_timestamp() RETURNING denial_count`, r.SessionID, threshold).Scan(&count); err != nil {
		return err
	}
	return appendAudit(ctx, tx, r.Hash, event, actor, boundedReason(reason), r.PolicyVersion)
}
