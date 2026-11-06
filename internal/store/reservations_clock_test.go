package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"fabric/internal/domain"
)

// Review F3: reservation decisions read the clock after the locks are taken.
// now() is the transaction start, so a hold or a close that waited on a lock
// decided with a stale time: a hold was granted to a lease that had expired
// during the wait and born already past expires_at, and a settlement started
// before expires_at committed after it.

// lockAttemptRow holds attempt a's row lock in another transaction until the
// returned func runs; it returns the DB clock at release.
func lockAttemptRow(t *testing.T, st *Store, ctx context.Context, a domain.AttemptID) func() time.Time {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM attempts WHERE id = $1 FOR UPDATE", string(a)); err != nil {
		t.Fatal(err)
	}
	return func() time.Time {
		var at time.Time
		if err := st.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return at
	}
}

func TestHoldReadsClockAfterLockWait(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rc", 10)

	// A lease that expires while the hold waits on the attempt lock is refused.
	f1 := liveAttempt(t, st, ctx, "m_ck1", "r_ck1")
	if _, err := st.pool.Exec(ctx, "UPDATE attempts SET lease_until = clock_timestamp() + interval '700 milliseconds' WHERE id = 'r_ck1'"); err != nil {
		t.Fatal(err)
	}
	unlock := lockAttemptRow(t, st, ctx, "r_ck1")
	done := make(chan error, 1)
	go func() { _, err := hold(st, ctx, "ck-1", "r_ck1", f1, time.Hour, item("rc", 2)); done <- err }()
	waitForLockWaiter(t, st, ctx, "attempts")
	time.Sleep(1200 * time.Millisecond)
	unlock()
	if err := <-done; !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("hold for a lease that expired during the lock wait: want a lease refusal, got %v", err)
	}
	if h := held(t, st, ctx, "rc"); h != 0 {
		t.Fatalf("held %d after a refused hold", h)
	}

	// expires_at counts the TTL from the decision, not from the transaction start.
	f2 := liveAttempt(t, st, ctx, "m_ck2", "r_ck2")
	unlock = lockAttemptRow(t, st, ctx, "r_ck2")
	type out struct {
		r   HoldResult
		err error
	}
	got := make(chan out, 1)
	go func() { r, err := hold(st, ctx, "ck-2", "r_ck2", f2, time.Second, item("rc", 2)); got <- out{r, err} }()
	waitForLockWaiter(t, st, ctx, "attempts")
	time.Sleep(1200 * time.Millisecond)
	released := unlock()
	o := <-got
	if o.err != nil {
		t.Fatal(o.err)
	}
	if exp := o.r.Reservations[0].ExpiresAt; exp.Before(released.Add(time.Second)) {
		t.Fatalf("expires_at %s is earlier than lock release %s + TTL 1s: the TTL ran during the wait", exp, released)
	}
}

func TestCloseReadsClockAfterLockWait(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rd", 10)

	// A settlement that waits on the attempt lock past expires_at is refused.
	f := liveAttempt(t, st, ctx, "m_ck3", "r_ck3")
	r, err := hold(st, ctx, "ck-3", "r_ck3", f, time.Second, item("rd", 2))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Reservations[0].ID
	unlock := lockAttemptRow(t, st, ctx, "r_ck3")
	done := make(chan error, 1)
	go func() {
		_, err := st.Settle(ctx, UsageReceipt{ReceiptID: "u-ck3", Reservation: id, Fence: f, Actual: 1})
		done <- err
	}()
	waitForLockWaiter(t, st, ctx, "attempts")
	time.Sleep(1500 * time.Millisecond)
	unlock()
	if err := <-done; !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("settlement that reached its locks after expires_at: want refused as expired, got %v", err)
	}

	// closed_at is stamped after the locks, not at the transaction start.
	r, err = hold(st, ctx, "ck-4", "r_ck3", f, time.Hour, item("rd", 1))
	if err == nil {
		t.Fatal("setup: second hold on a resource the attempt still holds was granted")
	}
	f5 := liveAttempt(t, st, ctx, "m_ck5", "r_ck5")
	r, err = hold(st, ctx, "ck-5", "r_ck5", f5, time.Hour, item("rd", 1))
	if err != nil {
		t.Fatal(err)
	}
	unlock = lockAttemptRow(t, st, ctx, "r_ck5")
	go func() { _, err := st.Release(ctx, r.Reservations[0].ID); done <- err }()
	waitForLockWaiter(t, st, ctx, "attempts")
	time.Sleep(500 * time.Millisecond)
	released := unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var closed time.Time
	if err := st.pool.QueryRow(ctx, "SELECT closed_at FROM reservations WHERE id = $1", r.Reservations[0].ID).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if closed.Before(released) {
		t.Fatalf("closed_at %s is before the lock was released at %s", closed, released)
	}
}
