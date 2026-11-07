package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// Reservations (migration 0009; Kimi P3.1 in fabric's shape).
//
// Lock order, never reversed, skipping classes an operation does not use:
// hold_commands row -> attempt row -> capacity rows sorted by resource ->
// reservation rows. A hold checks every resource after every lock is held
// and writes nothing unless all pass. Expiry and leases are decided by the
// database clock, read after the locks are held.

const (
	MaxHoldItems = 16
	MinHoldTTL   = time.Second
	MaxHoldTTL   = 24 * time.Hour
)

var (
	resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,62}$`)
	unitName     = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	receiptName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Reservation states.
const (
	ResHeld     = "HELD"
	ResSettled  = "SETTLED"
	ResReleased = "RELEASED"
	ResExpired  = "EXPIRED"
)

// HoldItem is one resource of a hold.
type HoldItem struct {
	Resource string `json:"resource"`
	Unit     string `json:"unit"`
	Amount   int64  `json:"amount"`
}

// HoldRequest asks for every item or none, for one attempt at its fence.
type HoldRequest struct {
	Key     string // client-minted idempotency key, never changed on retry
	Attempt domain.AttemptID
	Fence   int64
	Items   []HoldItem
	TTL     time.Duration
}

// Reservation is one held (or closed) amount of one resource.
type Reservation struct {
	ID         string           `json:"id"`
	Attempt    domain.AttemptID `json:"attempt"`
	Fence      int64            `json:"fence"`
	Resource   string           `json:"resource"`
	Unit       string           `json:"unit"`
	Amount     int64            `json:"amount"`
	Generation int              `json:"generation"`
	State      string           `json:"state"`
	ExpiresAt  time.Time        `json:"expires_at"`
}

// HoldResult is the stored, replayable answer to a hold.
type HoldResult struct {
	Status       string        `json:"status"` // held | refused
	Reason       string        `json:"reason,omitempty"`
	ErrKind      string        `json:"err_kind,omitempty"`
	Resource     string        `json:"resource,omitempty"` // the resource that refused, when one did
	Reservations []Reservation `json:"reservations,omitempty"`
	Replayed     bool          `json:"-"`
	Raw          []byte        `json:"-"` // the first response, verbatim
}

// HoldRefusal is returned for a refused hold; it unwraps to the domain sentinel.
type HoldRefusal struct {
	Result HoldResult
	Err    error
}

func (r *HoldRefusal) Error() string { return "hold refused: " + r.Result.Reason }
func (r *HoldRefusal) Unwrap() error { return r.Err }

// SetCapacity creates a resource or changes its capacity. The unit is fixed
// at creation; a capacity below what is held is refused (ErrInvalidTransition).
func (s *Store) SetCapacity(ctx context.Context, resource, unit string, capacity int64) error {
	if !resourceName.MatchString(resource) || !unitName.MatchString(unit) || capacity < 0 {
		return fmt.Errorf("%w: resource %q unit %q capacity %d", domain.ErrInvalidTransition, resource, unit, capacity)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	// Lock an existing row (the same lock a hold takes) and refuse a capacity
	// below what is held as a typed refusal (review F7), not a raw 23514.
	var held int64
	err = tx.QueryRow(ctx, "SELECT held FROM resource_capacities WHERE resource = $1 FOR UPDATE", resource).Scan(&held)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case capacity < held:
		return fmt.Errorf("%w: resource %s holds %d, more than capacity %d", domain.ErrInvalidTransition, resource, held, capacity)
	}
	var got string
	err = tx.QueryRow(ctx, `INSERT INTO resource_capacities (resource, unit, capacity) VALUES ($1, $2, $3)
		ON CONFLICT (resource) DO UPDATE SET capacity = EXCLUDED.capacity, version = resource_capacities.version + 1
		WHERE resource_capacities.unit = EXCLUDED.unit
		RETURNING unit`, resource, unit, capacity).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: resource %s has a different unit", domain.ErrInvalidTransition, resource)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Capacity reports a resource's capacity and what is held.
func (s *Store) Capacity(ctx context.Context, resource string) (capacity, held int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT capacity, held FROM resource_capacities WHERE resource = $1`, resource).Scan(&capacity, &held)
	return capacity, held, err
}

func normaliseHold(req HoldRequest) ([]HoldItem, string, error) {
	if _, err := domain.ParseCommandID(req.Key); err != nil {
		return nil, "", err
	}
	if _, err := domain.ParseAttemptID(string(req.Attempt)); err != nil {
		return nil, "", err
	}
	if req.Fence < 1 {
		return nil, "", fmt.Errorf("%w: fence %d", domain.ErrInvalidTransition, req.Fence)
	}
	if req.TTL < MinHoldTTL || req.TTL > MaxHoldTTL {
		return nil, "", fmt.Errorf("%w: ttl %s outside %s..%s", domain.ErrInvalidTransition, req.TTL, MinHoldTTL, MaxHoldTTL)
	}
	if len(req.Items) == 0 || len(req.Items) > MaxHoldItems {
		return nil, "", fmt.Errorf("%w: a hold names 1..%d resources", domain.ErrInvalidTransition, MaxHoldItems)
	}
	items := append([]HoldItem(nil), req.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].Resource < items[j].Resource })
	for i, it := range items {
		if !resourceName.MatchString(it.Resource) || !unitName.MatchString(it.Unit) {
			return nil, "", fmt.Errorf("%w: resource %q unit %q", domain.ErrInvalidTransition, it.Resource, it.Unit)
		}
		if it.Amount <= 0 {
			return nil, "", fmt.Errorf("%w: amount %d for %s must be positive", domain.ErrInvalidTransition, it.Amount, it.Resource)
		}
		if i > 0 && items[i-1].Resource == it.Resource {
			return nil, "", fmt.Errorf("%w: resource %s named twice", domain.ErrInvalidTransition, it.Resource)
		}
	}
	// The hash covers the sorted items, so the order a caller lists them in
	// does not matter; any other change under the same key is a conflict.
	canon, err := json.Marshal(struct {
		Attempt domain.AttemptID `json:"attempt"`
		Fence   int64            `json:"fence"`
		Items   []HoldItem       `json:"items"`
		TTLms   int64            `json:"ttl_ms"`
	}{req.Attempt, req.Fence, items, req.TTL.Milliseconds()})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(canon)
	return items, hex.EncodeToString(sum[:]), nil
}

func reservationID(key, resource string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + resource))
	return "v_" + hex.EncodeToString(sum[:12])
}

// Hold reserves every item for the attempt at its exact fence, or none.
//
// Same key and same request: the first answer, byte for byte, even if the
// resources are now full or free (a stored refusal replays too; retrying
// for a fresh decision needs a fresh key). Same key, different request:
// ErrIdempotencyConflict. A refusal leaves no reservation, event or
// capacity change and is returned as *HoldRefusal.
func (s *Store) Hold(ctx context.Context, req HoldRequest) (HoldResult, error) {
	items, hash, err := normaliseHold(req)
	if err != nil {
		return HoldResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return HoldResult{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	// 1. The command receipt. A concurrent first writer of the same key
	// blocks here until it commits, then this caller replays its answer.
	tag, err := tx.Exec(ctx, `INSERT INTO hold_commands (key, request_sha256) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, req.Key, hash)
	if err != nil {
		return HoldResult{}, err
	}
	if tag.RowsAffected() == 0 {
		var storedHash string
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT request_sha256, response_bytes FROM hold_commands WHERE key = $1 FOR UPDATE`, req.Key).Scan(&storedHash, &raw); err != nil {
			return HoldResult{}, err
		}
		if storedHash != hash {
			return HoldResult{}, fmt.Errorf("%w: hold %s", domain.ErrIdempotencyConflict, req.Key)
		}
		if raw == nil {
			return HoldResult{}, fmt.Errorf("hold %s has a receipt without a response", req.Key)
		}
		var res HoldResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return HoldResult{}, err
		}
		res.Replayed, res.Raw = true, raw
		if err := tx.Commit(ctx); err != nil {
			return HoldResult{}, err
		}
		return res, holdErr(res)
	}

	res := s.decideHold(ctx, tx, req, items)
	if res.Status == "" {
		return HoldResult{}, errors.New("hold: no decision")
	}
	if res.ErrKind == "error" {
		return HoldResult{}, errors.New(res.Reason)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return HoldResult{}, err
	}
	// 0014 allows this fill only in the transaction whose xid inserted the
	// row (xmin). Keep the INSERT and this UPDATE in the same (sub)transaction:
	// an INSERT inside a savepoint followed by a fill after RELEASE is refused
	// (23001), because xmin is then the subtransaction's xid. The receipt is a
	// replay record, not proof of a hold: AuthorizeDispatch checks the HELD
	// reservations themselves.
	if _, err := tx.Exec(ctx, `UPDATE hold_commands SET response_bytes = $2 WHERE key = $1`, req.Key, raw); err != nil {
		return HoldResult{}, err
	}
	if s.hooks.beforeHoldCommit != nil {
		if err := s.hooks.beforeHoldCommit(); err != nil {
			return HoldResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return HoldResult{}, err
	}
	res.Raw = raw
	return res, holdErr(res)
}

func holdErr(res HoldResult) error {
	if res.Status == "held" {
		return nil
	}
	sentinel := errKinds[res.ErrKind]
	if sentinel == nil {
		sentinel = domain.ErrInvalidTransition
	}
	return &HoldRefusal{Result: res, Err: sentinel}
}

// decideHold runs inside the hold transaction after the receipt row is
// owned. A refusal returns before any write; ErrKind "error" is an
// infrastructure failure that must abort the transaction.
func (s *Store) decideHold(ctx context.Context, tx pgx.Tx, req HoldRequest, items []HoldItem) HoldResult {
	fail := func(kind, resource, format string, a ...any) HoldResult {
		return HoldResult{Status: "refused", ErrKind: kind, Resource: resource, Reason: fmt.Sprintf(format, a...)}
	}
	broke := func(err error) HoldResult {
		return HoldResult{Status: "refused", ErrKind: "error", Reason: err.Error()}
	}

	// 2. The attempt, at its exact fence, live, lease unexpired by DB time.
	var fence int64
	var state string
	var leaseUntil *time.Time
	err := tx.QueryRow(ctx, `SELECT fence, state, lease_until FROM attempts WHERE id = $1 FOR UPDATE`,
		string(req.Attempt)).Scan(&fence, &state, &leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("not_found", "", "attempt %s does not exist", req.Attempt)
	}
	if err != nil {
		return broke(err)
	}
	if req.Fence != fence {
		return fail("stale_fence", "", "attempt %s is at fence %d, not %d", req.Attempt, fence, req.Fence)
	}
	switch domain.AttemptState(state) {
	case domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning:
	default:
		return fail("invalid_transition", "", "attempt %s is %s, not live", req.Attempt, state)
	}
	// 3. Every capacity row, in sorted order, before any check.
	type row struct {
		unit           string
		capacity, held int64
		found          bool
	}
	rows := make([]row, len(items))
	for i, it := range items {
		err := tx.QueryRow(ctx, `SELECT unit, capacity, held FROM resource_capacities WHERE resource = $1 FOR UPDATE`, it.Resource).
			Scan(&rows[i].unit, &rows[i].capacity, &rows[i].held)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return broke(err)
		default:
			rows[i].found = true
		}
		if s.hooks.holdLocked != nil {
			s.hooks.holdLocked(it.Resource)
		}
	}

	// The decision time is read after every lock is held (review F3): now()
	// is the transaction start, stale by however long the locks took.
	var decidedAt time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&decidedAt); err != nil {
		return broke(err)
	}
	if leaseUntil != nil && !leaseUntil.After(decidedAt) {
		return fail("invalid_transition", "", "attempt %s lease has expired", req.Attempt)
	}

	// 4. Check all; write nothing unless every resource passes.
	for i, it := range items {
		r := rows[i]
		if !r.found {
			return fail("not_found", it.Resource, "resource %s does not exist", it.Resource)
		}
		if r.unit != it.Unit {
			return fail("invalid_transition", it.Resource, "resource %s is in %s, not %s", it.Resource, r.unit, it.Unit)
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reservations WHERE attempt_id = $1 AND resource = $2 AND state = 'HELD')`,
			string(req.Attempt), it.Resource).Scan(&live); err != nil {
			return broke(err)
		}
		if live {
			return fail("attempt_live", it.Resource, "attempt %s already holds %s", req.Attempt, it.Resource)
		}
		if it.Amount > r.capacity-r.held { // no overflow: 0 <= held <= capacity
			return fail("over_capacity", it.Resource, "resource %s has %d of %d free, %d asked", it.Resource, r.capacity-r.held, r.capacity, it.Amount)
		}
	}

	// 5. Apply.
	res := HoldResult{Status: "held"}
	for _, it := range items {
		v := Reservation{ID: reservationID(req.Key, it.Resource), Attempt: req.Attempt, Fence: req.Fence, Resource: it.Resource,
			Unit: it.Unit, Amount: it.Amount, State: ResHeld}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(generation), 0) + 1 FROM reservations WHERE attempt_id = $1 AND resource = $2`,
			string(req.Attempt), it.Resource).Scan(&v.Generation); err != nil {
			return broke(err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $10::timestamptz, $10::timestamptz + $9::interval) RETURNING expires_at`,
			v.ID, req.Key, string(v.Attempt), v.Fence, v.Resource, v.Unit, v.Amount, v.Generation, pgInterval(req.TTL), decidedAt).Scan(&v.ExpiresAt); err != nil {
			return broke(err)
		}
		v.ExpiresAt = v.ExpiresAt.UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO reservation_events (reservation_id, n, state) VALUES ($1, 1, 'HELD')`, v.ID); err != nil {
			return broke(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE resource_capacities SET held = held + $2, version = version + 1 WHERE resource = $1`,
			it.Resource, it.Amount); err != nil {
			return broke(err)
		}
		res.Reservations = append(res.Reservations, v)
	}
	return res
}

// CloseResult says what a release, expiry or settlement did.
type CloseResult struct {
	Reservation Reservation
	Changed     bool // false: it was already closed (or, for expiry, not yet due)
}

// Release returns a held reservation's amount. Releasing a closed one is a
// no-op that reports its terminal state. Allowed after expires_at as long as
// the expiry has not been applied yet: then release is the one close.
func (s *Store) Release(ctx context.Context, id string) (CloseResult, error) {
	return s.closeReservation(ctx, id, ResReleased, nil)
}

// UsageReceipt is the actual usage of one reservation, under an external id.
type UsageReceipt struct {
	ReceiptID   string
	Reservation string
	Fence       int64 // the reservation's fence; another is refused
	Actual      int64
}

// Settle closes a held reservation with its actual usage. The same receipt
// again returns the first settlement; the receipt id reused with a different
// reservation or amount, or a second receipt for a settled reservation, is
// ErrIdempotencyConflict. Settlement after expires_at is refused.
func (s *Store) Settle(ctx context.Context, r UsageReceipt) (CloseResult, error) {
	if !receiptName.MatchString(r.ReceiptID) || r.Actual < 0 {
		return CloseResult{}, fmt.Errorf("%w: receipt %q actual %d", domain.ErrInvalidTransition, r.ReceiptID, r.Actual)
	}
	return s.closeReservation(ctx, r.Reservation, ResSettled, &r)
}

// ExpireReservations closes every held reservation whose expires_at has
// passed by DB time, each rechecked under its locks. Bounded by limit.
func (s *Store) ExpireReservations(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM reservations WHERE state = 'HELD' AND expires_at <= now()
		ORDER BY attempt_id, resource LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		res, err := s.closeReservation(ctx, id, ResExpired, nil)
		if err != nil {
			return n, err
		}
		if res.Changed {
			n++
		}
	}
	return n, nil
}

func (s *Store) closeReservation(ctx context.Context, id, to string, rcpt *UsageReceipt) (CloseResult, error) {
	var attempt, resource string
	// Unlocked read only to learn which rows to lock, in order.
	err := s.pool.QueryRow(ctx, `SELECT attempt_id, resource FROM reservations WHERE id = $1`, id).Scan(&attempt, &resource)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloseResult{}, fmt.Errorf("%w: reservation %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return CloseResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CloseResult{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT 1 FROM attempts WHERE id = $1 FOR UPDATE`, attempt); err != nil {
		return CloseResult{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM resource_capacities WHERE resource = $1 FOR UPDATE`, resource); err != nil {
		return CloseResult{}, err
	}
	var v Reservation
	var decidedAt time.Time
	err = tx.QueryRow(ctx, `SELECT id, attempt_id, fence, resource, unit, amount, generation, state, expires_at
		FROM reservations WHERE id = $1 FOR UPDATE`, id).
		Scan(&v.ID, &v.Attempt, &v.Fence, &v.Resource, &v.Unit, &v.Amount, &v.Generation, &v.State, &v.ExpiresAt)
	if err != nil {
		return CloseResult{}, err
	}
	// Due and closed_at use the clock after every lock is held (review F3).
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&decidedAt); err != nil {
		return CloseResult{}, err
	}
	due := !v.ExpiresAt.After(decidedAt)
	v.ExpiresAt = v.ExpiresAt.UTC()
	if s.hooks.reclaimLocked != nil {
		s.hooks.reclaimLocked(id)
	}

	if rcpt != nil {
		if rcpt.Fence != v.Fence {
			return CloseResult{}, fmt.Errorf("%w: reservation %s is at fence %d, not %d", domain.ErrStaleFence, id, v.Fence, rcpt.Fence)
		}
		var prevRes string
		var prevActual int64
		err := tx.QueryRow(ctx, `SELECT reservation_id, actual FROM usage_receipts WHERE resource = $1 AND receipt_id = $2`,
			v.Resource, rcpt.ReceiptID).Scan(&prevRes, &prevActual)
		switch {
		case err == nil:
			if prevRes != id || prevActual != rcpt.Actual {
				return CloseResult{}, fmt.Errorf("%w: receipt %s was recorded for %s with actual %d", domain.ErrIdempotencyConflict, rcpt.ReceiptID, prevRes, prevActual)
			}
			return CloseResult{Reservation: v}, tx.Commit(ctx) // the first settlement, unchanged
		case !errors.Is(err, pgx.ErrNoRows):
			return CloseResult{}, err
		}
	}

	if v.State != ResHeld {
		if rcpt != nil {
			if v.State == ResSettled {
				return CloseResult{}, fmt.Errorf("%w: reservation %s is already settled under another receipt", domain.ErrIdempotencyConflict, id)
			}
			return CloseResult{}, fmt.Errorf("%w: reservation %s is %s", domain.ErrInvalidTransition, id, v.State)
		}
		return CloseResult{Reservation: v}, tx.Commit(ctx)
	}
	switch to {
	case ResExpired:
		if !due {
			return CloseResult{Reservation: v}, tx.Commit(ctx) // not yet: renewed clocks never decide
		}
	case ResSettled:
		if due {
			return CloseResult{}, fmt.Errorf("%w: reservation %s expired before settlement", domain.ErrInvalidTransition, id)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE reservations SET state = $2, closed_at = $3 WHERE id = $1`, id, to, decidedAt); err != nil {
		return CloseResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reservation_events (reservation_id, n, state) VALUES ($1, 2, $2)`, id, to); err != nil {
		return CloseResult{}, err
	}
	if rcpt != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_receipts (resource, receipt_id, reservation_id, actual) VALUES ($1, $2, $3, $4)`,
			v.Resource, rcpt.ReceiptID, id, rcpt.Actual); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return CloseResult{}, fmt.Errorf("%w: receipt %s is already used", domain.ErrIdempotencyConflict, rcpt.ReceiptID)
			}
			return CloseResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE resource_capacities SET held = held - $2, version = version + 1 WHERE resource = $1`,
		v.Resource, v.Amount); err != nil {
		return CloseResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CloseResult{}, err
	}
	v.State = to
	return CloseResult{Reservation: v, Changed: true}, nil
}

// Reservations lists an attempt's reservations, oldest generation first.
func (s *Store) Reservations(ctx context.Context, attempt domain.AttemptID) ([]Reservation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, attempt_id, fence, resource, unit, amount, generation, state, expires_at
		FROM reservations WHERE attempt_id = $1 ORDER BY resource, generation`, string(attempt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var v Reservation
		if err := rows.Scan(&v.ID, &v.Attempt, &v.Fence, &v.Resource, &v.Unit, &v.Amount, &v.Generation, &v.State, &v.ExpiresAt); err != nil {
			return nil, err
		}
		v.ExpiresAt = v.ExpiresAt.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}
