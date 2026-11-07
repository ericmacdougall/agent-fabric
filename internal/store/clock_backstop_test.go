package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// Red-team F2 and the Kimi reservation-clock tranche, ported by intent.
// Cases 1, 3 and 6 are TestHoldReadsClockAfterLockWait and
// TestCloseReadsClockAfterLockWait; the rest are here.

// lockRow holds a FOR UPDATE row lock from another transaction until the
// returned func runs.
func lockRow(t *testing.T, st *Store, q string, args ...any) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func untilPast(t *testing.T, st *Store, at time.Time) {
	t.Helper()
	for {
		var past bool
		if err := st.pool.QueryRow(context.Background(), "SELECT clock_timestamp() > $1", at).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sqlCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// F2: a stored hold decision is written once.
func TestHoldReceiptIsWriteOnce(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rw", 0)
	f := liveAttempt(t, st, ctx, "m_hw", "r_hw")
	if _, err := hold(st, ctx, "hw-1", "r_hw", f, time.Minute, item("rw", 1)); err == nil {
		t.Fatal("setup: a hold over capacity was granted")
	}
	for _, q := range []string{
		`UPDATE hold_commands SET response_bytes = '{"status":"held"}' WHERE key = 'hw-1'`,
		`DELETE FROM hold_commands WHERE key = 'hw-1'`,
	} {
		if _, err := st.pool.Exec(ctx, q); sqlCode(err) != "23001" {
			t.Errorf("%s: want 23001, got %v", q, err)
		}
	}
	if _, err := hold(st, ctx, "hw-1", "r_hw", f, time.Minute, item("rw", 1)); !errors.Is(err, domain.ErrOverCapacity) {
		t.Fatalf("replay after the refused tampering: want the first refusal, got %v", err)
	}
}

// Case 2: a lease that expires while the hold waits on a capacity lock.
func TestHoldRechecksLeaseAfterCapacityLock(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rcl", 5)
	f := liveAttempt(t, st, ctx, "m_c2", "r_c2")
	if _, err := st.pool.Exec(ctx, "UPDATE attempts SET lease_until = clock_timestamp() + interval '700 milliseconds' WHERE id = 'r_c2'"); err != nil {
		t.Fatal(err)
	}
	unlock := lockRow(t, st, "SELECT 1 FROM resource_capacities WHERE resource = 'rcl' FOR UPDATE")
	done := make(chan error, 1)
	go func() { _, err := hold(st, ctx, "c2", "r_c2", f, time.Hour, item("rcl", 1)); done <- err }()
	waitForLockWaiter(t, st, ctx, "resource_capacities")
	time.Sleep(1200 * time.Millisecond)
	unlock()
	if err := <-done; !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("lease expired during the capacity wait: want refused, got %v", err)
	}
	if h := held(t, st, ctx, "rcl"); h != 0 {
		t.Fatalf("held %d", h)
	}
}

// Cases 4 and 5: the grant decides after the reservation locks: a lease, or
// an earlier reservation, that expires during the wait is a denial.
func TestAuthorizeRechecksAfterReservationLockWait(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 5)
	capacity(t, st, ctx, "rb", 5)

	f := acked(t, st, "m_c4", "r_c4", "ra", time.Hour)
	if _, err := st.pool.Exec(ctx, "UPDATE attempts SET lease_until = clock_timestamp() + interval '700 milliseconds' WHERE id = 'r_c4'"); err != nil {
		t.Fatal(err)
	}
	unlock := lockRow(t, st, "SELECT 1 FROM reservations WHERE attempt_id = 'r_c4' FOR UPDATE")
	got := make(chan Grant, 1)
	go func() { g, _ := st.AuthorizeDispatch(ctx, "r_c4", f, []string{"ra"}); got <- g }()
	waitForLockWaiter(t, st, ctx, "reservations")
	time.Sleep(1200 * time.Millisecond)
	unlock()
	if g := <-got; g.Granted || g.Reason != "attempt lease expired" {
		t.Fatalf("lease expired during the reservation wait: %+v", g)
	}

	f5 := liveAttempt(t, st, ctx, "m_c5", "r_c5")
	if _, err := hold(st, ctx, "c5-a", "r_c5", f5, MinHoldTTL, item("ra", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := hold(st, ctx, "c5-b", "r_c5", f5, time.Hour, item("rb", 1)); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "fabd", "ack:r_c5", domain.Command{Kind: domain.CmdAckStart, Mission: "m_c5", Attempt: "r_c5", Fence: f5})
	rs, _ := st.Reservations(ctx, "r_c5")
	unlock = lockRow(t, st, "SELECT 1 FROM reservations WHERE attempt_id = 'r_c5' AND resource = 'rb' FOR UPDATE")
	go func() { g, _ := st.AuthorizeDispatch(ctx, "r_c5", f5, []string{"ra", "rb"}); got <- g }()
	waitForLockWaiter(t, st, ctx, "reservations")
	untilPast(t, st, rs[0].ExpiresAt)
	unlock()
	if g := <-got; g.Granted || g.Reason != "reservation for ra expired" {
		t.Fatalf("an earlier reservation expired while waiting for a later one: %+v", g)
	}
}

// Case 7: an expiry that becomes due during its lock wait succeeds, stamped
// after the wait.
func TestExpireDecidesAfterLockWait(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "re", 5)
	f := liveAttempt(t, st, ctx, "m_c7", "r_c7")
	r, err := hold(st, ctx, "c7", "r_c7", f, MinHoldTTL, item("re", 1))
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockRow(t, st, "SELECT 1 FROM attempts WHERE id = 'r_c7' FOR UPDATE")
	done := make(chan CloseResult, 1)
	go func() { res, _ := st.closeReservation(ctx, r.Reservations[0].ID, ResExpired, nil); done <- res }()
	waitForLockWaiter(t, st, ctx, "attempts")
	untilPast(t, st, r.Reservations[0].ExpiresAt)
	unlock()
	if res := <-done; !res.Changed || res.Reservation.State != ResExpired {
		t.Fatalf("expiry due after the wait: %+v", res)
	}
}

// Case 8: the SQL backstops use the clock when the trigger runs, after a lock
// wait, and refuse a backdated decision time.
func TestReservationSQLBackstopsUseTriggerClock(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rs", 5)
	f := liveAttempt(t, st, ctx, "m_c8", "r_c8")
	if _, err := st.pool.Exec(ctx, "INSERT INTO hold_commands (key, request_sha256) VALUES ('c8-forged', 'x')"); err != nil {
		t.Fatal(err)
	}
	backdated := `INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, created_at, expires_at)
		VALUES ('rv_back', 'c8-forged', 'r_c8', ` + itoa(f) + `, 'rs', 'slots', 1, 1, now() - interval '1 hour', now() - interval '59 minutes')`
	if err := inTx(st, backdated, `INSERT INTO reservation_events (reservation_id, n, state) VALUES ('rv_back', 1, 'HELD')`,
		`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'rs'`); sqlCode(err) != "23514" {
		t.Errorf("backdated, born-expired insert: want 23514, got %v", err)
	}

	// Backdated but still live: only the decision-time check refuses it.
	if err := inTx(st, `INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, created_at, expires_at)
		VALUES ('rv_old', 'c8-forged', 'r_c8', `+itoa(f)+`, 'rs', 'slots', 1, 1, now() - interval '1 hour', now() + interval '1 minute')`,
		`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('rv_old', 1, 'HELD')`,
		`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'rs'`); sqlCode(err) != "23514" {
		t.Errorf("backdated decision time: want 23514, got %v", err)
	}
	// Decision time inside the transaction, but the transaction is older than
	// the TTL: expired before the INSERT runs.
	if err := inTx(st, "SELECT pg_sleep(1.2)", `INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, created_at, expires_at)
		VALUES ('rv_stale', 'c8-forged', 'r_c8', `+itoa(f)+`, 'rs', 'slots', 1, 1, now(), now() + interval '1 second')`,
		`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('rv_stale', 1, 'HELD')`,
		`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'rs'`); sqlCode(err) != "23514" {
		t.Errorf("insert expired by the time it runs: want 23514, got %v", err)
	}

	r, err := hold(st, ctx, "c8", "r_c8", f, MinHoldTTL, item("rs", 1))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Reservations[0].ID
	if err := inTx(st, `UPDATE reservations SET state = 'EXPIRED', closed_at = now() WHERE id = '`+id+`'`,
		`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('`+id+`', 2, 'EXPIRED')`,
		`UPDATE resource_capacities SET held = held - 1 WHERE resource = 'rs'`); sqlCode(err) != "23514" {
		t.Errorf("forged expiry before expires_at: want 23514, got %v", err)
	}
	// A forged settlement that waits on the row past expires_at.
	unlock := lockRow(t, st, "SELECT 1 FROM reservations WHERE id = $1 FOR UPDATE", id)
	done := make(chan error, 1)
	go func() {
		done <- inTx(st, `UPDATE reservations SET state = 'SETTLED', closed_at = now() WHERE id = '`+id+`'`,
			`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('`+id+`', 2, 'SETTLED')`,
			`INSERT INTO usage_receipts (resource, receipt_id, reservation_id, actual) VALUES ('rs', 'u-c8', '`+id+`', 1)`,
			`UPDATE resource_capacities SET held = held - 1 WHERE resource = 'rs'`)
	}()
	waitForLockWaiter(t, st, ctx, "reservations")
	untilPast(t, st, r.Reservations[0].ExpiresAt)
	unlock()
	if err := <-done; sqlCode(err) != "23514" {
		t.Errorf("forged settlement after a wait past expires_at: want 23514, got %v", err)
	}
	if h := held(t, st, ctx, "rs"); h != 1 {
		t.Fatalf("held %d after refused forgeries, want 1", h)
	}
}

// Case 9 (control): replays and an explicit release still work after expiry.
func TestReplayAndReleaseAfterExpiry(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rp", 5)
	f := acked(t, st, "m_c9", "r_c9", "rp", MinHoldTTL)
	g, err := st.AuthorizeDispatch(ctx, "r_c9", f, []string{"rp"})
	if err != nil || !g.Granted {
		t.Fatalf("grant: %+v %v", g, err)
	}
	rs, _ := st.Reservations(ctx, "r_c9")
	untilPast(t, st, rs[0].ExpiresAt)
	if again, err := hold(st, ctx, "hold:r_c9", "r_c9", f, MinHoldTTL, item("rp", 1)); err != nil || !again.Replayed {
		t.Fatalf("hold replay after expiry: %+v %v", again, err)
	}
	if again, err := st.AuthorizeDispatch(ctx, "r_c9", f, []string{"rp"}); err != nil || !again.Replayed || !again.Granted {
		t.Fatalf("grant replay after expiry: %+v %v", again, err)
	}
	if res, err := st.Release(ctx, rs[0].ID); err != nil || !res.Changed || res.Reservation.State != ResReleased {
		t.Fatalf("explicit release after expiry, before the sweep: %+v %v", res, err)
	}
}
