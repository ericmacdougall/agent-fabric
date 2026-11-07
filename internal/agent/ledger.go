package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// Spend resources are measured in micro-USD.
const SpendUnit = "usd_micro"

// Cap is one budget a request must fit under (monthly, per job, per attempt).
type Cap struct {
	Resource string
	Total    int64 // micro-USD
}

// ErrBudget means a hold would exceed a cap; nothing was sent.
type ErrBudget struct{ Resource string }

func (e *ErrBudget) Error() string { return "budget exhausted on " + e.Resource }

// SpendResource names a per-subject spend resource ("job", "run") safely.
func SpendResource(kind, id string) string {
	h := sha256.Sum256([]byte(kind + "/" + id))
	return "spend." + kind + "." + hex.EncodeToString(h[:12])
}

// Ledger turns the store's reservations into cumulative spend caps. A store
// reservation bounds what is held at once; a spend cap also has to remember
// what was already spent, so before every hold the capacity is set to
// total minus spent (settled receipts plus crash-expired holds charged at
// their held amount). Hold, settle and the capacity refresh run under a
// per-resource advisory lock so a settle cannot slip between the refresh
// and the hold.
type Ledger struct {
	St      *store.Store
	Attempt domain.AttemptID
	Fence   int64
	Caps    []Cap
	TTL     time.Duration
}

func (l *Ledger) resources() []string {
	rs := make([]string, 0, len(l.Caps))
	for _, c := range l.Caps {
		rs = append(rs, c.Resource)
	}
	sort.Strings(rs)
	return rs
}

func (l *Ledger) lock(ctx context.Context) (func(), error) {
	conn, err := l.St.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	rs := l.resources()
	for i, r := range rs {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 7))`, r); err != nil {
			for _, p := range rs[:i] {
				conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 7))`, p) //nolint:errcheck
			}
			conn.Release()
			return nil, err
		}
	}
	return func() {
		for _, r := range rs {
			conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 7))`, r) //nolint:errcheck
		}
		conn.Release()
	}, nil
}

// Spent is what a resource has consumed: settled actuals plus expired holds.
func (l *Ledger) Spent(ctx context.Context, resource string) (int64, error) {
	var n int64
	err := l.St.Pool().QueryRow(ctx, `SELECT
		coalesce((SELECT sum(actual) FROM usage_receipts WHERE resource = $1), 0) +
		coalesce((SELECT sum(amount) FROM reservations WHERE resource = $1 AND state = 'EXPIRED'), 0)`, resource).Scan(&n)
	return n, err
}

// Hold reserves amount against every cap, all or none, before any byte is sent.
func (l *Ledger) Hold(ctx context.Context, key string, amount int64) ([]store.Reservation, error) {
	if amount <= 0 {
		return nil, errors.New("hold amount must be positive")
	}
	unlock, err := l.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var items []store.HoldItem
	for _, c := range l.Caps {
		spent, err := l.Spent(ctx, c.Resource)
		if err != nil {
			return nil, err
		}
		remaining := c.Total - spent
		if remaining < amount {
			return nil, &ErrBudget{Resource: c.Resource}
		}
		if err := l.St.SetCapacity(ctx, c.Resource, SpendUnit, remaining); err != nil {
			if errors.Is(err, domain.ErrInvalidTransition) {
				return nil, &ErrBudget{Resource: c.Resource}
			}
			return nil, err
		}
		items = append(items, store.HoldItem{Resource: c.Resource, Unit: SpendUnit, Amount: amount})
	}
	res, err := l.St.Hold(ctx, store.HoldRequest{Key: key, Attempt: l.Attempt, Fence: l.Fence, Items: items, TTL: l.TTL})
	var refusal *store.HoldRefusal
	if errors.As(err, &refusal) && refusal.Result.ErrKind == "over_capacity" {
		return nil, &ErrBudget{Resource: refusal.Result.Resource}
	}
	if err != nil {
		return nil, err
	}
	return res.Reservations, nil
}

func receiptID(res store.Reservation) string {
	h := sha256.Sum256([]byte(res.ID))
	return "ag-" + hex.EncodeToString(h[:16])
}

// Settle closes holds with the actual cost.
func (l *Ledger) Settle(ctx context.Context, rs []store.Reservation, actual int64) error {
	unlock, err := l.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	for _, r := range rs {
		if _, err := l.St.Settle(ctx, store.UsageReceipt{ReceiptID: receiptID(r), Reservation: r.ID, Fence: r.Fence, Actual: actual}); err != nil {
			return fmt.Errorf("settle %s: %w", r.Resource, err)
		}
	}
	return nil
}

// Reap charges every hold a dead process left HELD for this attempt at its
// held amount: the request may have been sent and billed, and the usage is
// unknown, so the worst case stands.
func (l *Ledger) Reap(ctx context.Context) (int, error) {
	all, err := l.St.Reservations(ctx, l.Attempt)
	if err != nil {
		return 0, err
	}
	byAmount := map[int64][]store.Reservation{}
	for _, r := range all {
		if r.State == store.ResHeld {
			byAmount[r.Amount] = append(byAmount[r.Amount], r)
		}
	}
	n := 0
	for amt, rs := range byAmount {
		if err := l.Settle(ctx, rs, amt); err != nil {
			if !errors.Is(err, domain.ErrInvalidTransition) {
				return n, err
			}
			// Past its expiry a hold cannot be settled; expiring it books the
			// held amount as spent (Spent counts EXPIRED holds).
			if _, xerr := l.St.ExpireReservations(ctx, 100); xerr != nil {
				return n, xerr
			}
		}
		n += len(rs)
	}
	return n, nil
}
