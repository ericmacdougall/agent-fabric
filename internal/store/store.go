// Package store is the fabric's single-writer authority over Postgres
// (DESIGN.md 3.2, 5.3). Every mission mutation is a Command executed inside
// one transaction that: serialises on the mission row, replays a stored
// response for a repeated idempotency key, runs the pure reducer, appends the
// events with a gap-free per-mission sequence, writes the outbox rows and the
// state snapshot, maintains the attempts table, and only then commits.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
	"fabric/internal/reducer"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store owns a pool and the lease policy.
type Store struct {
	pool  *pgxpool.Pool
	Lease time.Duration // how long a freshly started attempt holds its lease without a heartbeat

	// hooks are crash cutpoints for tests (export_test.go). nil in production.
	hooks hooks

	repairWakeTimeout time.Duration // per-wake deadline in RepairAskWakes; 0 means defaultRepairWakeTimeout

	// dispatchSlots caps effect dispatches holding a pool connection across
	// every broker on this store (review nit 3 on cbebbd6).
	dispatchSlotsOnce sync.Once
	dispatchSlots     chan struct{}
}

type hooks struct {
	beforeCommit func(cmd domain.Command) error // returning an error aborts the transaction
	afterCommit  func(cmd domain.Command)       // runs after COMMIT and before the response is returned

	// Reservation barriers (reservations_test.go).
	holdLocked       func(resource string) // after each capacity row lock in Hold
	grantLocked      func()                // after the locks in AuthorizeDispatch, before the clock read
	beforeHoldCommit func() error          // returning an error aborts the hold transaction
	reclaimLocked    func(id string)       // after the reservation row lock in release/expire/settle

	afterEventsRead func() // in VerifiedSnapshot, between the event read and the snapshot read
}

// Open connects; it does not migrate.
func Open(ctx context.Context, dsn string) (*Store, error) {
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
	st := &Store{pool: pool, Lease: 5 * time.Minute}
	if err := st.refuseNewerSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return st, nil
}

// ErrNewerSchema: the database holds a migration this binary does not embed.
var ErrNewerSchema = errors.New("database schema is newer than this binary")

// embeddedMigrations is the binary's own migration list, in apply order.
func embeddedMigrations() ([]string, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// refuseNewerSchema is the forward guard (goodqa#15087, main WAL
// w5a7fb2daf13): a binary refuses a database that a newer binary has
// migrated, naming its own latest migration and the first foreign one, so
// an old fabd never runs against rules it does not know. It binds at
// Open and Migrate only; a binary already running is stopped by the
// database's own triggers instead.
func (s *Store) refuseNewerSchema(ctx context.Context) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass('public.fabric_schema') IS NOT NULL").Scan(&exists); err != nil || !exists {
		return err
	}
	names, err := embeddedMigrations()
	if err != nil {
		return err
	}
	var foreign string
	err = s.pool.QueryRow(ctx, "SELECT name FROM fabric_schema WHERE NOT (name = ANY($1)) ORDER BY name LIMIT 1", names).Scan(&foreign)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	latest := ""
	if len(names) > 0 {
		latest = names[len(names)-1]
	}
	return fmt.Errorf("%w: it holds %s, which this binary (latest %s) does not embed; upgrade this binary", ErrNewerSchema, foreign, latest)
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the pool for read-only audits and tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// DispatchSlots is the store-wide semaphore for effect dispatches: half the
// pool, at least one, so reconcile and commands always have connections.
func (s *Store) DispatchSlots() chan struct{} {
	s.dispatchSlotsOnce.Do(func() {
		n := int(s.pool.Config().MaxConns) / 2
		if n < 1 {
			n = 1
		}
		s.dispatchSlots = make(chan struct{}, n)
	})
	return s.dispatchSlots
}

const migrationLock = 7_431_902_001 // arbitrary, fixed advisory lock key for migrations

// Migrate applies embedded migrations in name order under an advisory lock.
// It is idempotent and roll-forward only.
func (s *Store) Migrate(ctx context.Context) error { return s.migrateUpTo(ctx, "") }

// migrateUpTo applies migrations whose name sorts at or before upTo (all of
// them when upTo is empty). Tests use it to build an older schema.
func (s *Store) migrateUpTo(ctx context.Context, upTo string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLock) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS fabric_schema (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	// Review R1 on 3f38cc3: record each migration's checksum and refuse one
	// that changed after it was applied. A row from before checksums (NULL)
	// is stamped with the current file, trusted on first sight.
	if _, err := conn.Exec(ctx, `ALTER TABLE fabric_schema ADD COLUMN IF NOT EXISTS sha256 text`); err != nil {
		return err
	}
	if err := s.refuseNewerSchema(ctx); err != nil {
		return err
	}
	names, err := embeddedMigrations()
	if err != nil {
		return err
	}
	for _, name := range names {
		if upTo != "" && name > upTo {
			break
		}
		sql, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(sql)
		want := hex.EncodeToString(sum[:])
		var recorded *string
		err = conn.QueryRow(ctx, "SELECT sha256 FROM fabric_schema WHERE name = $1", name).Scan(&recorded)
		if err == nil {
			if recorded == nil {
				if _, err := conn.Exec(ctx, "UPDATE fabric_schema SET sha256 = $2 WHERE name = $1 AND sha256 IS NULL", name, want); err != nil {
					return err
				}
				continue
			}
			if *recorded != want {
				return fmt.Errorf("migration %s changed after it was applied (recorded %.12s, file %.12s): ship a new migration instead", name, *recorded, want)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO fabric_schema (name, sha256) VALUES ($1, $2)", name, want); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// CreateMission registers a mission in IDLE with its provider, which never
// changes (goodqa#15087). Idempotent on the id; a second call naming another
// provider for an existing id is refused.
func (s *Store) CreateMission(ctx context.Context, id domain.MissionID, provider domain.Provider, ticket, repo, branch string) error {
	if _, err := domain.ParseMissionID(string(id)); err != nil {
		return err
	}
	if _, err := domain.ParseProvider(string(provider)); err != nil {
		return err
	}
	if ticket == "" {
		return fmt.Errorf("%w: a mission must name a ticket (R5)", domain.ErrInvalidTransition)
	}
	state, err := json.Marshal(domain.NewMission(id))
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO missions (id, ticket, repo, branch, state, provider)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`, string(id), ticket, repo, branch, state, string(provider))
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	return s.checkProvider(ctx, id, provider)
}

// checkProvider refuses an existing mission whose provider differs.
func (s *Store) checkProvider(ctx context.Context, id domain.MissionID, provider domain.Provider) error {
	var have string
	if err := s.pool.QueryRow(ctx, "SELECT provider FROM missions WHERE id = $1", string(id)).Scan(&have); err != nil {
		return err
	}
	if domain.Provider(have) != provider {
		return fmt.Errorf("%w: mission %s is %s, not %s", domain.ErrInvalidProvider, id, have, provider)
	}
	return nil
}

// SetCap changes the fleet cap (R1). The scheduler reads it under lock.
// SlotCap returns the global admission cap.
func (s *Store) SlotCap(ctx context.Context) (int, error) {
	var c int
	err := s.pool.QueryRow(ctx, "SELECT cap FROM slots WHERE id = 1").Scan(&c)
	return c, err
}

func (s *Store) SetCap(ctx context.Context, cap int) error {
	_, err := s.pool.Exec(ctx, "UPDATE slots SET cap = $1 WHERE id = 1", cap)
	return err
}

// MaxCommandBytes bounds a whole command at the door (review RV-8): every
// free-text field, not only Request, is capped before anything is stored.
const MaxCommandBytes = 64 * 1024

// Response is the stored, replayable answer to a command.
type Response struct {
	Status   string         `json:"status"` // ok | refused
	Reason   string         `json:"reason,omitempty"`
	ErrKind  string         `json:"err_kind,omitempty"`
	Events   []domain.Event `json:"events,omitempty"`
	FirstSeq int64          `json:"first_seq,omitempty"`
	LastSeq  int64          `json:"last_seq,omitempty"`
	Version  domain.Version `json:"version"`
	Replayed bool           `json:"-"` // served from the commands table; the reducer did not run
	// Raw is the response exactly as first stored. A replay returns the same
	// bytes (lost-response protocol, DESIGN.md 3.1); rows from before
	// migration 0008 replay the jsonb form instead.
	Raw []byte `json:"-"`
}

// errKinds maps stable refusal kinds to their sentinel so a replayed refusal
// still satisfies errors.Is for the caller.
var errKinds = map[string]error{
	"stale_fence":        domain.ErrStaleFence,
	"over_capacity":      domain.ErrOverCapacity,
	"unknown_unresolved": domain.ErrUnknownUnresolved,
	"evidence_stale":     domain.ErrEvidenceStale,
	"generation_stale":   domain.ErrGenerationStale,
	"duplicate":          domain.ErrDuplicate,
	"not_found":          domain.ErrNotFound,
	"attempt_live":       domain.ErrAttemptLive,
	"live_test_missing":  domain.ErrLiveTestMissing,
	"invalid_transition": domain.ErrInvalidTransition,
}

func kindOf(err error) string {
	for k, e := range errKinds {
		if errors.Is(err, e) {
			return k
		}
	}
	return "invalid_transition"
}

// Refusal is the error returned for a refused command: it unwraps to the
// domain sentinel and carries the stored response.
type Refusal struct {
	Response Response
	Err      error
}

func (r *Refusal) Error() string { return "refused: " + r.Response.Reason }
func (r *Refusal) Unwrap() error { return r.Err }

// Execute runs one command under its idempotency key (actor, mission, id).
//
// Same key and same request hash: the stored response is returned and the
// reducer does not run. Same key and a different hash: ErrIdempotencyConflict,
// nothing stored. A refusal is stored like a success and returned as *Refusal.
func (s *Store) Execute(ctx context.Context, actor domain.ActorID, commandID domain.CommandID, cmd domain.Command) (Response, error) {
	if _, err := domain.ParseActorID(string(actor)); err != nil {
		return Response{}, err
	}
	if _, err := domain.ParseCommandID(string(commandID)); err != nil {
		return Response{}, err
	}
	if _, err := domain.ParseMissionID(string(cmd.Mission)); err != nil {
		return Response{}, err
	}
	if cmd.Attempt != "" {
		if _, err := domain.ParseAttemptID(string(cmd.Attempt)); err != nil {
			return Response{}, err
		}
	}
	if cmd.Effect != "" {
		if _, err := domain.ParseEffectID(string(cmd.Effect)); err != nil {
			return Response{}, err
		}
	}
	if cmd.Host != "" {
		if _, err := domain.ParseHostID(string(cmd.Host)); err != nil {
			return Response{}, err
		}
	}
	if whole, _ := json.Marshal(cmd); len(whole) > MaxCommandBytes {
		return Response{}, fmt.Errorf("%w: command larger than %d bytes", domain.ErrInvalidTransition, MaxCommandBytes)
	}
	// F1 (R25): bound the request body before anything is stored, so an
	// oversized request under a stale fence cannot land in stale_observations.
	if len(cmd.Request) > domain.MaxRequestBytes {
		return Response{}, fmt.Errorf("%w: request larger than %d bytes", domain.ErrInvalidTransition, domain.MaxRequestBytes)
	}
	if len(cmd.Request) > 0 && !json.Valid(cmd.Request) {
		return Response{}, fmt.Errorf("%w: request is not valid JSON", domain.ErrInvalidTransition)
	}
	if err := checkAskReserved(actor, commandID, cmd); err != nil {
		return Response{}, err
	}
	if err := checkSchedulerReserved(actor, cmd); err != nil {
		return Response{}, err
	}
	canon := cmd.CanonicalRequest()
	sum := sha256.Sum256(canon)
	hash := hex.EncodeToString(sum[:])

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Response{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	// Admissions serialise on the slots row so the cap is a transaction, not a
	// warning (AC-1). Everything else serialises on the mission row only.
	var cap int
	if cmd.Kind == domain.CmdAdmitMission {
		if err := tx.QueryRow(ctx, "SELECT cap FROM slots WHERE id = 1 FOR UPDATE").Scan(&cap); err != nil {
			return Response{}, fmt.Errorf("slots: %w", err)
		}
	}

	var stateJSON []byte
	var provider string
	err = tx.QueryRow(ctx, "SELECT state, provider FROM missions WHERE id = $1 FOR UPDATE", string(cmd.Mission)).Scan(&stateJSON, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return Response{}, fmt.Errorf("%w: mission %s", domain.ErrNotFound, cmd.Mission)
	}
	if err != nil {
		return Response{}, err
	}

	// Replay check.
	var storedHash, storedFormat string
	var storedResp, storedBytes []byte
	err = tx.QueryRow(ctx, `SELECT request_sha256, request_format, response, response_bytes FROM commands
		WHERE actor = $1 AND mission_id = $2 AND command_id = $3`, string(actor), string(cmd.Mission), string(commandID)).Scan(&storedHash, &storedFormat, &storedResp, &storedBytes)
	switch {
	case err == nil:
		if storedHash != hash {
			return Response{}, fmt.Errorf("%w: command %s", domain.ErrIdempotencyConflict, commandID)
		}
		raw, err := replayBytes(storedFormat, storedResp, storedBytes)
		if err != nil {
			return Response{}, fmt.Errorf("command %s: %w", commandID, err)
		}
		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			return Response{}, err
		}
		resp.Replayed = true
		resp.Raw = raw
		if err := tx.Commit(ctx); err != nil {
			return Response{}, err
		}
		if resp.Status == "refused" {
			return resp, &Refusal{Response: resp, Err: errKinds[resp.ErrKind]}
		}
		return resp, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return Response{}, err
	}

	// Red-team F1: a log holding an event kind this build cannot read is not
	// written to. Execute decides from the snapshot, which cannot show what
	// this build does not know, so appending would build on an unread past.
	// Nothing is stored; the caller parks the mission.
	var foreign string
	switch err := tx.QueryRow(ctx, `SELECT kind FROM events WHERE mission_id = $1 AND NOT (kind = ANY($2)) LIMIT 1`,
		string(cmd.Mission), domain.KnownEventKinds()).Scan(&foreign); {
	case err == nil:
		return Response{}, fmt.Errorf("%w: mission %s holds %q; this build does not write to it", domain.ErrUnsupportedEvent, cmd.Mission, foreign)
	case !errors.Is(err, pgx.ErrNoRows):
		return Response{}, err
	}

	// Review F1: an ask wake needs an answered ask of this mission. Checked
	// after the replay lookup (a stored wake already passed it) and never
	// stored, so a wake forged before the answer cannot poison the fixed key.
	if cmd.Kind == domain.CmdWake && isAskToken(cmd.Token) {
		var state, owner string
		err := tx.QueryRow(ctx, "SELECT state, COALESCE(mission_id, '') FROM asks WHERE id = $1 FOR SHARE",
			strings.TrimPrefix(cmd.Token, askTokenPrefix)).Scan(&state, &owner)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (state != "answered" || owner != string(cmd.Mission))) {
			return Response{}, fmt.Errorf("%w: %s is not an answered ask of mission %s", domain.ErrInvalidTransition, cmd.Token, cmd.Mission)
		}
		if err != nil {
			return Response{}, err
		}
	}

	var m domain.Mission
	if err := json.Unmarshal(stateJSON, &m); err != nil {
		return Response{}, fmt.Errorf("mission %s snapshot: %w", cmd.Mission, err)
	}
	var preRefusal error // a refusal decided before the reducer; stored like any other (S5)
	if cmd.Kind == domain.CmdExpireLease {
		// Authority rechecked before the mutation (review H10 / RV-22): a
		// lease renewed after the sweep read its page is not expired.
		var fresh *bool
		err := tx.QueryRow(ctx, "SELECT lease_until >= now() FROM attempts WHERE id = $1 FOR UPDATE", string(cmd.Attempt)).Scan(&fresh)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Response{}, err
		}
		if fresh != nil && *fresh {
			preRefusal = fmt.Errorf("%w: lease renewed after the sweep read it", domain.ErrInvalidTransition)
		}
	}

	env := reducer.Env{Cap: cap, Operator: domain.IsOperator(actor), Provider: domain.Provider(provider)}
	if cmd.Kind == domain.CmdCompleteMission || cmd.Kind == domain.CmdUnpauseMission {
		// Review N1: the broker's effects for this mission that are not
		// settled, in this transaction. A new intent or dispatch holds its
		// live attempt row (0020), and done needs no live attempt, so none
		// can appear after this count (review O3).
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM effects WHERE mission_id = $1
			AND state NOT IN ('CONFIRMED', 'FAILED_DEFINITE', 'NOT_APPLIED', 'RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED')`, string(cmd.Mission)).Scan(&env.UnsettledEffects); err != nil {
			return Response{}, err
		}
	}
	if cmd.Kind == domain.CmdAdmitMission {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM missions WHERE id <> $1
			AND execution IN ('ADMITTED', 'RUNNING', 'WAITING', 'PAUSED')`, string(cmd.Mission)).Scan(&env.SlotsUsed); err != nil {
			return Response{}, err
		}
	}

	var events []domain.Event
	var derr error
	if preRefusal != nil {
		derr = preRefusal
	} else {
		events, derr = reducer.Decide(m, cmd, env)
	}
	resp := Response{Status: "ok", Version: m.Version}
	if derr != nil {
		resp.Status = "refused"
		resp.Reason = derr.Error()
		resp.ErrKind = kindOf(derr)
		if errors.Is(derr, domain.ErrStaleFence) {
			payload, _ := json.Marshal(cmd)
			if _, err := tx.Exec(ctx, `INSERT INTO stale_observations
				(mission_id, attempt_id, fence, current_fence, actor, command_id, kind, payload)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				string(cmd.Mission), string(cmd.Attempt), cmd.Fence, m.Version.AttemptFence,
				string(actor), string(commandID), string(cmd.Kind), payload); err != nil {
				return Response{}, err
			}
		}
	} else {
		for i := range events {
			events[i].Seq = m.Seq + 1
			payload, err := json.Marshal(events[i])
			if err != nil {
				return Response{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, fence, actor, command_id, payload)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				string(cmd.Mission), events[i].Seq, string(events[i].Kind), events[i].Fence, string(actor), string(commandID), payload); err != nil {
				return Response{}, fmt.Errorf("append event: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4)`,
				string(cmd.Mission), events[i].Seq, string(events[i].Kind), payload); err != nil {
				return Response{}, fmt.Errorf("outbox: %w", err)
			}
			if err := s.applyAttemptRow(ctx, tx, events[i]); err != nil {
				return Response{}, err
			}
			m = reducer.Apply(m, events[i])
		}
		if len(events) > 0 {
			resp.FirstSeq = events[0].Seq
			resp.LastSeq = events[len(events)-1].Seq
		}
		resp.Events = events
		resp.Version = m.Version
		snapshot, err := json.Marshal(m)
		if err != nil {
			return Response{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE missions SET seq = $2, intent_revision = $3, attempt_fence = $4,
			candidate_generation = $5, execution = $6, candidate = $7, delivery = $8, terminal = $9,
			state = $10, updated_at = now(),
			not_before = CASE WHEN $11::bigint > 0 THEN now() + $11::bigint * interval '1 millisecond'
				WHEN $11::bigint < 0 THEN NULL ELSE not_before END
			WHERE id = $1`,
			string(m.ID), m.Seq, m.Version.IntentRevision, m.Version.AttemptFence, m.Version.CandidateGeneration,
			string(m.Execution), string(m.Candidate), string(m.Delivery), string(m.Terminal), snapshot, deferredMS(events)); err != nil {
			return Response{}, err
		}
	}

	respJSON, err := json.Marshal(resp)
	if err != nil {
		return Response{}, err
	}
	var firstSeq, lastSeq *int64
	if resp.FirstSeq > 0 {
		firstSeq, lastSeq = &resp.FirstSeq, &resp.LastSeq
	}
	if _, err := tx.Exec(ctx, `INSERT INTO commands
		(actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes, first_seq, last_seq,
		 canonical_request, request_format)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'CANONICAL_V1')`,
		string(actor), string(cmd.Mission), string(commandID), string(cmd.Kind), hash, resp.Status, respJSON, respJSON, firstSeq, lastSeq,
		canon); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Cannot happen under the mission row lock; keep it a loud error.
			return Response{}, fmt.Errorf("%w: concurrent same-key write escaped the mission lock", domain.ErrDuplicate)
		}
		return Response{}, err
	}
	if s.hooks.beforeCommit != nil {
		if err := s.hooks.beforeCommit(cmd); err != nil {
			return Response{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Response{}, err
	}
	if s.hooks.afterCommit != nil {
		s.hooks.afterCommit(cmd)
	}
	resp.Raw = respJSON
	if derr != nil {
		return resp, &Refusal{Response: resp, Err: derr}
	}
	return resp, nil
}

// applyAttemptRow keeps the attempts table in step with ATTEMPT_* events in
// the same transaction.
func (s *Store) applyAttemptRow(ctx context.Context, tx pgx.Tx, e domain.Event) error {
	switch e.Kind {
	case domain.EvAttemptStarted:
		// path and lock_path come from the launcher's command (goodqa#15087);
		// the attempts_path trigger checks them against missions.provider.
		_, err := tx.Exec(ctx, `INSERT INTO attempts (id, mission_id, fence, revision, generation, host, role, state, lease_until, path, lock_path)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + $9::interval, $10, NULLIF($11, ''))`,
			string(e.Attempt), string(e.Mission), e.Fence, e.Revision, e.Generation, string(e.Host), e.Role,
			string(domain.AttemptLeased), pgInterval(s.Lease), string(e.Path), e.LockPath)
		return err
	case domain.EvAttemptAcked:
		tag, err := tx.Exec(ctx, `UPDATE attempts SET state = $3, lease_until = now() + $4::interval
			WHERE id = $1 AND fence = $2`, string(e.Attempt), e.Fence, string(e.AttemptTo), pgInterval(s.Lease))
		return oneAttemptRow(tag, err, e)
	case domain.EvAttemptFinished, domain.EvAttemptUnknown, domain.EvAttemptReconciled:
		tag, err := tx.Exec(ctx, `UPDATE attempts SET state = $3, lease_until = NULL, ended_at = now()
			WHERE id = $1 AND fence = $2`, string(e.Attempt), e.Fence, string(e.AttemptTo))
		return oneAttemptRow(tag, err, e)
	}
	return nil
}

// oneAttemptRow fails the command when an attempt update touched anything
// but exactly one row (S5): the table and the events have diverged, and the
// transaction must roll back rather than append an unreflected event.
func oneAttemptRow(tag pgconn.CommandTag, err error, e domain.Event) error {
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("attempt %s at fence %d: %s updated %d rows, want 1", e.Attempt, e.Fence, e.Kind, n)
	}
	return nil
}

func pgInterval(d time.Duration) string {
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}

// Heartbeat extends a live attempt's lease. A wrong fence is ErrStaleFence; an
// attempt that is no longer live is ErrInvalidTransition.
func (s *Store) Heartbeat(ctx context.Context, attempt domain.AttemptID, fence int64) error {
	return s.heartbeat(ctx, attempt, fence, false)
}

// HeartbeatLive extends the lease only while it has not lapsed by database
// time (goodqa#15087 V1): a resumed native run whose lease already expired
// is refused and left to reconcile, never revived.
func (s *Store) HeartbeatLive(ctx context.Context, attempt domain.AttemptID, fence int64) error {
	return s.heartbeat(ctx, attempt, fence, true)
}

func (s *Store) heartbeat(ctx context.Context, attempt domain.AttemptID, fence int64, requireLive bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE attempts SET lease_until = now() + $3::interval
		WHERE id = $1 AND fence = $2 AND state IN ('LEASED', 'PREPARING', 'RUNNING')
		  AND (NOT $4 OR (lease_until IS NOT NULL AND lease_until >= now()))`,
		string(attempt), fence, pgInterval(s.Lease), requireLive)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var curFence int64
	var state string
	err = s.pool.QueryRow(ctx, "SELECT fence, state FROM attempts WHERE id = $1", string(attempt)).Scan(&curFence, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return err
	}
	if curFence != fence {
		return fmt.Errorf("%w: heartbeat fence %d, attempt holds %d", domain.ErrStaleFence, fence, curFence)
	}
	if requireLive && (state == "LEASED" || state == "PREPARING" || state == "RUNNING") {
		return fmt.Errorf("%w: lease of %s has lapsed", domain.ErrInvalidTransition, attempt)
	}
	return fmt.Errorf("%w: heartbeat while %s", domain.ErrInvalidTransition, state)
}

// ExpiredLease is one attempt whose lease ran out by database time.
type ExpiredLease struct {
	Attempt domain.AttemptID
	Mission domain.MissionID
	Fence   int64
}

// ExpireLeases moves at most limit expired live attempts to UNKNOWN through the
// ordinary command path with a deterministic command id, so a sweep that is
// retried or run twice is a replay, never a second transition. Bounded page:
// a backlog can never become one unbounded transaction.
func (s *Store) ExpireLeases(ctx context.Context, limit int) ([]ExpiredLease, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, mission_id, fence, (extract(epoch FROM lease_until) * 1000)::bigint FROM attempts
		WHERE state IN ('LEASED', 'PREPARING', 'RUNNING') AND lease_until < now()
		ORDER BY lease_until LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	type row struct {
		ExpiredLease
		leaseMillis int64
	}
	var page []row
	for rows.Next() {
		var e row
		if err := rows.Scan(&e.Attempt, &e.Mission, &e.Fence, &e.leaseMillis); err != nil {
			rows.Close()
			return nil, err
		}
		page = append(page, e)
	}
	rows.Close()
	var expired []ExpiredLease
	var failures []error
	for _, e := range page {
		cmd := domain.Command{Kind: domain.CmdExpireLease, Mission: e.Mission, Attempt: e.Attempt, Fence: e.Fence}
		// Fixed-size key (review H10 / RV-21): the commands key already
		// scopes it to the mission; the observed lease makes a renewed lease
		// a new sweep attempt instead of a replayed refusal.
		id := domain.CommandID(fmt.Sprintf("expire:f%d:%d", e.Fence, e.leaseMillis))
		if _, err := s.Execute(ctx, "fabd", id, cmd); err != nil {
			var ref *Refusal
			if errors.As(err, &ref) {
				continue // moved by a concurrent sweep, renewed, or finished meanwhile
			}
			failures = append(failures, fmt.Errorf("expire %s: %w", e.Attempt, err))
			continue // one bad row never stalls the page (review RV-21)
		}
		expired = append(expired, e.ExpiredLease)
	}
	return expired, errors.Join(failures...)
}

// Mission loads the snapshot.
func (s *Store) Mission(ctx context.Context, id domain.MissionID) (domain.Mission, error) {
	var stateJSON []byte
	err := s.pool.QueryRow(ctx, "SELECT state FROM missions WHERE id = $1", string(id)).Scan(&stateJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Mission{}, fmt.Errorf("%w: mission %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return domain.Mission{}, err
	}
	var m domain.Mission
	return m, json.Unmarshal(stateJSON, &m)
}

// Events returns a mission's log in sequence order.
func (s *Store) Events(ctx context.Context, id domain.MissionID) ([]domain.Event, error) {
	return readEvents(ctx, s.pool, id)
}

func readEvents(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, id domain.MissionID) ([]domain.Event, error) {
	rows, err := q.Query(ctx, "SELECT payload FROM events WHERE mission_id = $1 ORDER BY seq", string(id))
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
		var e domain.Event
		if err := json.Unmarshal(payload, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifiedSnapshot replays the log and compares it with the snapshot, both
// read in one read-only REPEATABLE READ transaction (Kimi projection
// analysis): a writer committing between two separate reads made a valid
// history look corrupt. It returns the rebuilt mission and the log it came
// from, one high-water. A digest mismatch is a bug, returned with both digests.
func (s *Store) VerifiedSnapshot(ctx context.Context, id domain.MissionID) (domain.Mission, []domain.Event, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Mission{}, nil, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // read-only
	events, err := readEvents(ctx, tx, id)
	if err != nil {
		return domain.Mission{}, nil, err
	}
	if s.hooks.afterEventsRead != nil {
		s.hooks.afterEventsRead()
	}
	var stateJSON []byte
	err = tx.QueryRow(ctx, "SELECT state FROM missions WHERE id = $1", string(id)).Scan(&stateJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Mission{}, nil, fmt.Errorf("%w: mission %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return domain.Mission{}, nil, err
	}
	var snap domain.Mission
	if err := json.Unmarshal(stateJSON, &snap); err != nil {
		return domain.Mission{}, nil, fmt.Errorf("mission %s snapshot: %w", id, err)
	}
	rebuilt, err := reducer.Replay(id, events)
	if err != nil {
		// Never serve the known prefix as current state (Kimi compatibility analysis).
		return domain.Mission{}, nil, fmt.Errorf("mission %s: %w", id, err)
	}
	if a, b := reducer.Digest(rebuilt), reducer.Digest(snap); a != b {
		return rebuilt, events, fmt.Errorf("mission %s: replay digest %s != snapshot %s", id, a[:12], b[:12])
	}
	return rebuilt, events, tx.Commit(ctx)
}

// Rebuild is VerifiedSnapshot without the log.
func (s *Store) Rebuild(ctx context.Context, id domain.MissionID) (domain.Mission, error) {
	m, _, err := s.VerifiedSnapshot(ctx, id)
	return m, err
}

// OutboxRow is one event for a consumer.
type OutboxRow struct {
	ID      int64
	Mission domain.MissionID
	Seq     int64
	Event   domain.Event
}

// OutboxPage returns up to limit rows after the consumer's cursor.
func (s *Store) OutboxPage(ctx context.Context, consumer string, limit int) ([]OutboxRow, error) {
	// Rows whose transaction is still in progress (or newer than one that is)
	// are not visible yet, so an id that commits late can never be skipped
	// (review H5 / RV-3).
	rows, err := s.pool.Query(ctx, `SELECT o.id, o.mission_id, o.seq, o.payload FROM outbox o
		WHERE o.id > COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = $1), 0)
		  AND o.xid < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY o.id LIMIT $2`, consumer, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Mission, &r.Seq, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &r.Event); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AckOutbox advances the consumer's cursor; it never moves backwards, so a
// repeated or out-of-order ack is harmless.
func (s *Store) AckOutbox(ctx context.Context, consumer string, upto int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, $2)
		ON CONFLICT (consumer) DO UPDATE SET last_id = GREATEST(outbox_cursors.last_id, EXCLUDED.last_id), updated_at = now()`,
		consumer, upto)
	return err
}

// SlotsHeld counts missions holding a slot, for tests and status.
func (s *Store) SlotsHeld(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM missions WHERE execution IN ('ADMITTED', 'RUNNING', 'WAITING', 'PAUSED')`).Scan(&n)
	return n, err
}

// replayBytes picks the stored first response by the row's request format,
// failing closed on a format this build does not know (Kimi receipt.go).
// CANONICAL_V1 rows always carry their bytes (0011 CHECK); a legacy row has
// them only if it was written between 0008 and 0011.
func replayBytes(format string, jsonb, raw []byte) ([]byte, error) {
	switch format {
	case "CANONICAL_V1":
		if raw == nil {
			return nil, errors.New("canonical row without response bytes")
		}
		return raw, nil
	case "LEGACY_HASH_ONLY":
		if raw == nil {
			return jsonb, nil // written before 0008
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown request format %q", format)
	}
}

// Truncate empties every fabric table except the schema ledger. Tests only.
func (s *Store) Truncate(ctx context.Context) error {
	// The append-only tables refuse TRUNCATE unless this transaction opts in (0010).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	for _, q := range []string{
		"SET LOCAL fabric.allow_truncate = 'on'",
		`TRUNCATE commands, events, outbox, outbox_cursors, attempts, stale_observations, missions,
		dispatch_grants, usage_receipts, reservation_events, reservations, hold_commands, resource_capacities RESTART IDENTITY CASCADE`,
		"UPDATE slots SET cap = 4 WHERE id = 1",
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports a Postgres unique-constraint error.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// quoteIdent is used by tests to build database names safely.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// deferredMS is the backoff of a DISPATCH_DEFERRED among events, -1 when
// the mission is (re-)admitted (the backoff is cleared), or 0 (unchanged).
func deferredMS(events []domain.Event) int64 {
	for _, e := range events {
		switch e.Kind {
		case domain.EvDispatchDeferred:
			return e.BackoffMS
		case domain.EvMissionAdmitted:
			return -1
		}
	}
	return 0
}
