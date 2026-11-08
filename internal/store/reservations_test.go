package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// The reservation tests are adversarial: each one asserts the clean outcome
// (an exact count, a typed refusal, zero partial rows), so a broken lock or
// check that falls through to the database backstop as an error still fails.

// liveAttempt admits mission m and starts attempt a; it returns the fence.
func liveAttempt(t *testing.T, st *Store, ctx context.Context, m domain.MissionID, a domain.AttemptID) int64 {
	t.Helper()
	mk(t, st, ctx, m)
	ok(t, st, ctx, "chat", "admit-"+string(m), domain.Command{Kind: domain.CmdAdmitMission, Mission: m})
	return startAttempt(t, st, ctx, m, a)
}

func capacity(t *testing.T, st *Store, ctx context.Context, res string, cap int64) {
	t.Helper()
	if err := st.SetCapacity(ctx, res, "slots", cap); err != nil {
		t.Fatalf("capacity %s: %v", res, err)
	}
}

func held(t *testing.T, st *Store, ctx context.Context, res string) int64 {
	t.Helper()
	_, h, err := st.Capacity(ctx, res)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// barrier returns a hook that makes up to n callers wait for each other, or
// until the timeout (callers blocked on a lock never arrive, by design).
func barrier(n int, timeout time.Duration) func() {
	var mu sync.Mutex
	arrived := 0
	return func() {
		mu.Lock()
		arrived++
		mu.Unlock()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			mu.Lock()
			all := arrived >= n
			mu.Unlock()
			if all {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func item(res string, n int64) HoldItem { return HoldItem{Resource: res, Unit: "slots", Amount: n} }

func hold(st *Store, ctx context.Context, key string, a domain.AttemptID, fence int64, ttl time.Duration, items ...HoldItem) (HoldResult, error) {
	return st.Hold(ctx, HoldRequest{Key: key, Attempt: a, Fence: fence, Items: items, TTL: ttl})
}

func refusedAs(t *testing.T, err error, kind error, resource string) HoldResult {
	t.Helper()
	var ref *HoldRefusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a clean refusal (%v on %q), got %v", kind, resource, err)
	}
	if !errors.Is(err, kind) || ref.Result.Resource != resource {
		t.Fatalf("refusal %+v: want %v on %q", ref.Result, kind, resource)
	}
	return ref.Result
}

// NEG-C: two (here eight) 7-unit holds against capacity 10 -> exactly one.
// The barrier holds the winner inside its lock so the others pile up on it;
// without the capacity row lock they all read held=0.
func TestHoldCapacityRace(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.SetCap(ctx, 32); err != nil {
		t.Fatal(err)
	}
	capacity(t, st, ctx, "host:t2:workers", 10)
	const n = 8
	fences := make([]int64, n)
	for i := 0; i < n; i++ {
		fences[i] = liveAttempt(t, st, ctx, domain.MissionID(fmt.Sprintf("m_cr%d", i)), domain.AttemptID(fmt.Sprintf("r_cr%d", i)))
	}
	b := barrier(n, 300*time.Millisecond)
	st.hooks.holdLocked = func(string) { b() }
	var wg sync.WaitGroup
	var won, lost int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := hold(st, ctx, fmt.Sprintf("cr-%d", i), domain.AttemptID(fmt.Sprintf("r_cr%d", i)), fences[i], time.Minute, item("host:t2:workers", 7))
			var ref *HoldRefusal
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.As(err, &ref) && errors.Is(err, domain.ErrOverCapacity):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("holder %d: not a clean outcome: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	st.hooks.holdLocked = nil
	if won != 1 || lost != n-1 {
		t.Fatalf("won=%d lost=%d, want 1/%d", won, lost, n-1)
	}
	if h := held(t, st, ctx, "host:t2:workers"); h != 7 {
		t.Fatalf("held %d, want 7", h)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations`); c != 1 {
		t.Fatalf("reservations %d, want 1 (losers must leave no row)", c)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservation_events`); c != 1 {
		t.Fatalf("events %d, want 1", c)
	}
}

// Two multi-resource holds naming the resources in opposite orders, each
// paused after its first lock: sorted locking means no deadlock, ever.
func TestHoldOppositeOrderNoDeadlock(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 100)
	capacity(t, st, ctx, "rb", 100)
	f1 := liveAttempt(t, st, ctx, "m_dl1", "r_dl1")
	f2 := liveAttempt(t, st, ctx, "m_dl2", "r_dl2")
	for round := 0; round < 5; round++ {
		b := barrier(2, 150*time.Millisecond)
		var first sync.Map
		st.hooks.holdLocked = func(res string) {
			if _, seen := first.LoadOrStore(res, true); !seen {
				b()
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		results := make([]HoldResult, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			results[0], errs[0] = hold(st, ctx, fmt.Sprintf("dl1-%d", round), "r_dl1", f1, time.Minute, item("ra", 1), item("rb", 1))
		}()
		go func() {
			defer wg.Done()
			results[1], errs[1] = hold(st, ctx, fmt.Sprintf("dl2-%d", round), "r_dl2", f2, time.Minute, item("rb", 1), item("ra", 1))
		}()
		wg.Wait()
		st.hooks.holdLocked = nil
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d holder %d: %v", round, i, err)
			}
		}
		for _, r := range results {
			for _, v := range r.Reservations {
				if _, err := st.Release(ctx, v.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if held(t, st, ctx, "ra") != 0 || held(t, st, ctx, "rb") != 0 {
		t.Fatal("capacity not returned after release")
	}
}

// NEG partial failure: one resource short refuses the whole hold, names the
// short resource, and leaves the other resource untouched.
func TestHoldAllOrNothing(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	capacity(t, st, ctx, "rb", 10)
	f := liveAttempt(t, st, ctx, "m_an1", "r_an1")
	g := liveAttempt(t, st, ctx, "m_an2", "r_an2")
	if _, err := hold(st, ctx, "an-fill", "r_an2", g, time.Minute, item("rb", 8)); err != nil {
		t.Fatal(err)
	}
	snapshot := func() [3]int {
		return [3]int{count(t, st, ctx, `SELECT count(*) FROM reservations`), count(t, st, ctx, `SELECT count(*) FROM reservation_events`),
			count(t, st, ctx, `SELECT count(*)::int FROM resource_capacities WHERE held > 0`)}
	}
	before := snapshot()
	_, err := hold(st, ctx, "an-1", "r_an1", f, time.Minute, item("ra", 5), item("rb", 5))
	refusedAs(t, err, domain.ErrOverCapacity, "rb")
	_, err = hold(st, ctx, "an-2", "r_an1", f, time.Minute, item("ra", 5), item("rz", 1))
	refusedAs(t, err, domain.ErrNotFound, "rz")
	_, err = hold(st, ctx, "an-3", "r_an1", f, time.Minute, item("ra", 5), HoldItem{Resource: "rb", Unit: "usd_micros", Amount: 1})
	refusedAs(t, err, domain.ErrInvalidTransition, "rb")
	if after := snapshot(); after != before || held(t, st, ctx, "ra") != 0 {
		t.Fatalf("a refused hold changed state: %v -> %v, ra held %d", before, after, held(t, st, ctx, "ra"))
	}
	// Exactly at the boundary passes.
	if _, err := hold(st, ctx, "an-4", "r_an1", f, time.Minute, item("ra", 10), item("rb", 2)); err != nil {
		t.Fatalf("exact fit refused: %v", err)
	}
	// Bad input is refused before any row: duplicates, zero, bad ttl.
	for i, bad := range []HoldRequest{
		{Key: "an-b1", Attempt: "r_an1", Fence: f, TTL: time.Minute, Items: []HoldItem{item("ra", 1), item("ra", 1)}},
		{Key: "an-b2", Attempt: "r_an1", Fence: f, TTL: time.Minute, Items: []HoldItem{item("ra", 0)}},
		{Key: "an-b3", Attempt: "r_an1", Fence: f, TTL: time.Millisecond, Items: []HoldItem{item("ra", 1)}},
		{Key: "an-b4", Attempt: "r_an1", Fence: f, TTL: time.Minute},
	} {
		if _, err := st.Hold(ctx, bad); err == nil {
			t.Fatalf("bad request %d accepted", i)
		}
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM hold_commands WHERE key LIKE 'an-b%'`); c != 0 {
		t.Fatalf("bad requests left %d receipts", c)
	}
}

// NEG-D: a hold names the attempt's exact fence, on a live attempt with an
// unexpired lease. Older, newer and reused fences are refused with no row.
func TestHoldFenceBinding(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_fb", "r_fb1")
	for i, fence := range []int64{f - 1, f + 1} {
		if fence < 1 {
			continue
		}
		_, err := hold(st, ctx, fmt.Sprintf("fb-%d", i), "r_fb1", fence, time.Minute, item("ra", 1))
		refusedAs(t, err, domain.ErrStaleFence, "")
	}
	_, err := hold(st, ctx, "fb-x", "r_nope", f, time.Minute, item("ra", 1))
	refusedAs(t, err, domain.ErrNotFound, "")

	// The attempt ends; a new attempt takes fence f+1. The old attempt at its
	// own (old) fence is no longer live; the new attempt with the old fence is stale.
	ok(t, st, ctx, "host", "fb-fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_fb", Attempt: "r_fb1", Fence: f, Outcome: "checkpoint"})
	ok(t, st, ctx, "fabd", "fb-rec", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_fb", Attempt: "r_fb1", Fence: f, Result: "VERIFIED"})
	_, err = hold(st, ctx, "fb-old", "r_fb1", f, time.Minute, item("ra", 1))
	refusedAs(t, err, domain.ErrInvalidTransition, "")
	f2 := startAttempt(t, st, ctx, "m_fb", "r_fb2")
	if f2 == f {
		t.Fatalf("setup: fence not advanced (%d)", f2)
	}
	_, err = hold(st, ctx, "fb-reuse", "r_fb2", f, time.Minute, item("ra", 1))
	refusedAs(t, err, domain.ErrStaleFence, "")

	// A lease expired by DB time, not yet swept, is not live either.
	if _, err := st.pool.Exec(ctx, `UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = 'r_fb2'`); err != nil {
		t.Fatal(err)
	}
	_, err = hold(st, ctx, "fb-lease", "r_fb2", f2, time.Minute, item("ra", 1))
	refusedAs(t, err, domain.ErrInvalidTransition, "")
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations`); c != 0 {
		t.Fatalf("refused holds left %d reservations", c)
	}
}

// Keyed replay: the first answer byte for byte, even after the world moved.
func TestHoldReplay(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.SetCap(ctx, 32); err != nil {
		t.Fatal(err)
	}
	capacity(t, st, ctx, "ra", 10)
	capacity(t, st, ctx, "rb", 10)
	f := liveAttempt(t, st, ctx, "m_rp1", "r_rp1")
	g := liveAttempt(t, st, ctx, "m_rp2", "r_rp2")
	first, err := hold(st, ctx, "rp-1", "r_rp1", f, time.Minute, item("ra", 6), item("rb", 1))
	if err != nil {
		t.Fatal(err)
	}
	// Reordered items under the same key are the same request.
	again, err := hold(st, ctx, "rp-1", "r_rp1", f, time.Minute, item("rb", 1), item("ra", 6))
	if err != nil || !again.Replayed || !bytes.Equal(again.Raw, first.Raw) {
		t.Fatalf("replay: %v replayed=%v equal=%v", err, again.Replayed, bytes.Equal(again.Raw, first.Raw))
	}
	for i, changed := range [][]HoldItem{{item("ra", 7), item("rb", 1)}, {item("ra", 6)}} {
		if _, err := hold(st, ctx, "rp-1", "r_rp1", f, time.Minute, changed...); !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("changed request %d under the same key: %v", i, err)
		}
	}
	if _, err := hold(st, ctx, "rp-1", "r_rp1", f, 2*time.Minute, item("ra", 6), item("rb", 1)); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("changed ttl under the same key: %v", err)
	}
	// A lost response replays as the original hold even once the capacity is
	// full (Kimi plan 7.7, TestReservationLostResponseDoesNotDoubleHold).
	h := liveAttempt(t, st, ctx, "m_rp3", "r_rp3")
	if _, err := hold(st, ctx, "rp-fill", "r_rp3", h, time.Minute, item("ra", 4)); err != nil {
		t.Fatal(err)
	}
	full, err := hold(st, ctx, "rp-1", "r_rp1", f, time.Minute, item("ra", 6), item("rb", 1))
	if err != nil || !full.Replayed || !bytes.Equal(full.Raw, first.Raw) {
		t.Fatalf("replay with ra full: %v replayed=%v equal=%v", err, full.Replayed, bytes.Equal(full.Raw, first.Raw))
	}
	if h := held(t, st, ctx, "ra"); h != 10 {
		t.Fatalf("ra held %d after a replay at full capacity, want 10", h)
	}
	// A refusal replays as the same refusal after capacity frees up.
	_, err = hold(st, ctx, "rp-2", "r_rp2", g, time.Minute, item("ra", 5))
	refusedAs(t, err, domain.ErrOverCapacity, "ra")
	for _, v := range first.Reservations {
		if _, err := st.Release(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	res, err := hold(st, ctx, "rp-2", "r_rp2", g, time.Minute, item("ra", 5))
	if r := refusedAs(t, err, domain.ErrOverCapacity, "ra"); !res.Replayed && !r.Replayed {
		t.Fatal("refusal was decided again instead of replayed")
	}
	// Sixteen racers on one fresh key: one reservation set, identical bytes.
	const n = 16
	raws := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r HoldResult
			r, errs[i] = hold(st, ctx, "rp-race", "r_rp2", g, time.Minute, item("rb", 3))
			raws[i] = r.Raw
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || !bytes.Equal(raws[i], raws[0]) {
			t.Fatalf("racer %d: %v (bytes equal=%v)", i, errs[i], bytes.Equal(raws[i], raws[0]))
		}
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE hold_key = 'rp-race'`); c != 1 || held(t, st, ctx, "rb") != 3 {
		t.Fatalf("racers made %d reservations, rb held %d", c, held(t, st, ctx, "rb"))
	}
}

// A crash before commit stores nothing; the retry under the same key holds once.
func TestHoldCrashBeforeCommit(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_cc", "r_cc")
	cut := errors.New("cut")
	st.hooks.beforeHoldCommit = func() error { return cut }
	if _, err := hold(st, ctx, "cc-1", "r_cc", f, time.Minute, item("ra", 4)); !errors.Is(err, cut) {
		t.Fatalf("cut: %v", err)
	}
	st.hooks.beforeHoldCommit = nil
	if c := count(t, st, ctx, `SELECT count(*) FROM hold_commands`); c != 0 || held(t, st, ctx, "ra") != 0 {
		t.Fatalf("a cut hold left %d receipts, held %d", c, held(t, st, ctx, "ra"))
	}
	if _, err := hold(st, ctx, "cc-1", "r_cc", f, time.Minute, item("ra", 4)); err != nil {
		t.Fatal(err)
	}
	if held(t, st, ctx, "ra") != 4 {
		t.Fatal("retry did not hold exactly once")
	}
}

// One live generation per attempt and resource; the next only after a close.
func TestHoldGenerations(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_gn", "r_gn")
	r1, err := hold(st, ctx, "gn-1", "r_gn", f, time.Minute, item("ra", 2))
	if err != nil || r1.Reservations[0].Generation != 1 {
		t.Fatalf("gen 1: %+v %v", r1, err)
	}
	_, err = hold(st, ctx, "gn-2", "r_gn", f, time.Minute, item("ra", 2))
	refusedAs(t, err, domain.ErrAttemptLive, "ra")
	if _, err := st.Release(ctx, r1.Reservations[0].ID); err != nil {
		t.Fatal(err)
	}
	r3, err := hold(st, ctx, "gn-3", "r_gn", f, time.Minute, item("ra", 2))
	if err != nil || r3.Reservations[0].Generation != 2 {
		t.Fatalf("gen 2: %+v %v", r3, err)
	}
}

// Expiry is decided by DB time and closes once; release before the sweep
// is the one close; settlement after expiry is refused.
func TestReservationExpiry(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	capacity(t, st, ctx, "rb", 10)
	f := liveAttempt(t, st, ctx, "m_ex", "r_ex")
	r, err := hold(st, ctx, "ex-1", "r_ex", f, time.Second, item("ra", 3), item("rb", 4))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.ExpireReservations(ctx, 10); err != nil || n != 0 {
		t.Fatalf("early sweep expired %d (%v)", n, err)
	}
	// Even a direct close as EXPIRED before expires_at changes nothing.
	if res, err := st.closeReservation(ctx, r.Reservations[0].ID, ResExpired, nil); err != nil || res.Changed {
		t.Fatalf("early expiry close: %+v %v", res, err)
	}
	time.Sleep(1200 * time.Millisecond)
	// ra: release after expires_at but before the sweep is the one close.
	if res, err := st.Release(ctx, r.Reservations[0].ID); err != nil || !res.Changed {
		t.Fatalf("late release: %+v %v", res, err)
	}
	// rb: settlement after expiry is refused.
	if _, err := st.Settle(ctx, UsageReceipt{ReceiptID: "u-ex", Reservation: r.Reservations[1].ID, Fence: f, Actual: 1}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("settle after expiry: %v", err)
	}
	if n, err := st.ExpireReservations(ctx, 10); err != nil || n != 1 {
		t.Fatalf("sweep expired %d (%v), want 1 (rb only)", n, err)
	}
	if n, err := st.ExpireReservations(ctx, 10); err != nil || n != 0 {
		t.Fatalf("second sweep expired %d (%v)", n, err)
	}
	if held(t, st, ctx, "ra") != 0 || held(t, st, ctx, "rb") != 0 {
		t.Fatal("capacity not returned exactly once")
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservation_events WHERE n = 2`); c != 2 {
		t.Fatalf("terminal events %d, want 2", c)
	}
}

// Racing closers on one reservation, before and after expiry: exactly one
// close, capacity returned once, no closer sees an error it should not.
func TestReservationCloseRace(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_rc", "r_rc")
	for _, late := range []bool{false, true} {
		ttl := time.Minute
		if late {
			ttl = time.Second
		}
		r, err := hold(st, ctx, fmt.Sprintf("rc-%v", late), "r_rc", f, ttl, item("ra", 5))
		if err != nil {
			t.Fatal(err)
		}
		id := r.Reservations[0].ID
		if late {
			time.Sleep(1200 * time.Millisecond)
		}
		const n = 9
		b := barrier(n, 150*time.Millisecond)
		st.hooks.reclaimLocked = func(string) { b() }
		var changed int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var res CloseResult
				var err error
				switch i % 3 {
				case 0:
					res, err = st.Release(ctx, id)
				case 1:
					var k int
					k, err = st.ExpireReservations(ctx, 10)
					res.Changed = k > 0
				case 2:
					res, err = st.Settle(ctx, UsageReceipt{ReceiptID: fmt.Sprintf("u-rc-%v-%d", late, i), Reservation: id, Fence: f, Actual: 2})
					// A losing settle is a typed refusal, never anything else.
					if err != nil && (errors.Is(err, domain.ErrIdempotencyConflict) || errors.Is(err, domain.ErrInvalidTransition)) {
						err = nil
					}
				}
				if err != nil {
					t.Errorf("late=%v closer %d: %v", late, i, err)
				}
				if res.Changed {
					atomic.AddInt32(&changed, 1)
				}
			}(i)
		}
		wg.Wait()
		st.hooks.reclaimLocked = nil
		if changed != 1 {
			t.Fatalf("late=%v: %d closers changed the reservation, want 1", late, changed)
		}
		if h := held(t, st, ctx, "ra"); h != 0 {
			t.Fatalf("late=%v: held %d after the race", late, h)
		}
		if c := count(t, st, ctx, `SELECT count(*) FROM reservation_events WHERE reservation_id = $1`, id); c != 2 {
			t.Fatalf("late=%v: %d events, want 2", late, c)
		}
		if late {
			if c := count(t, st, ctx, `SELECT count(*) FROM usage_receipts WHERE reservation_id = $1`, id); c != 0 {
				t.Fatal("an expired reservation was settled")
			}
		}
	}
}

// NEG-E: duplicate receipts replay; conflicting ones are refused.
func TestSettleReceipts(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_st1", "r_st1")
	g := liveAttempt(t, st, ctx, "m_st2", "r_st2")
	r1, _ := hold(st, ctx, "st-1", "r_st1", f, time.Minute, item("ra", 4))
	r2, _ := hold(st, ctx, "st-2", "r_st2", g, time.Minute, item("ra", 4))
	id1, id2 := r1.Reservations[0].ID, r2.Reservations[0].ID

	if _, err := st.Settle(ctx, UsageReceipt{ReceiptID: "u-1", Reservation: id1, Fence: f + 1, Actual: 3}); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("settle at the wrong fence: %v", err)
	}
	// Eight racers settle the same receipt: one changes, seven replay.
	var changed int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := st.Settle(ctx, UsageReceipt{ReceiptID: "u-1", Reservation: id1, Fence: f, Actual: 3})
			if err != nil {
				t.Errorf("duplicate settle: %v", err)
			}
			if res.Changed {
				atomic.AddInt32(&changed, 1)
			}
			if res.Reservation.State != ResSettled {
				t.Errorf("settle answered state %s", res.Reservation.State)
			}
		}()
	}
	wg.Wait()
	if changed != 1 {
		t.Fatalf("%d settles changed the reservation, want 1", changed)
	}
	events := count(t, st, ctx, `SELECT count(*) FROM reservation_events`)
	for i, bad := range []UsageReceipt{
		{ReceiptID: "u-1", Reservation: id1, Fence: f, Actual: 4}, // same receipt, different actual
		{ReceiptID: "u-9", Reservation: id1, Fence: f, Actual: 3}, // second receipt for a settled one
		{ReceiptID: "u-1", Reservation: id2, Fence: g, Actual: 3}, // receipt reused for another reservation
	} {
		if _, err := st.Settle(ctx, bad); !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("conflicting receipt %d: %v", i, err)
		}
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservation_events`); c != events || held(t, st, ctx, "ra") != 4 {
		t.Fatalf("conflicts changed state: events %d -> %d, held %d", events, c, held(t, st, ctx, "ra"))
	}
	if _, err := st.Release(ctx, id1); err != nil {
		t.Fatalf("release of a settled reservation must be a no-op: %v", err)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM usage_receipts`); c != 1 {
		t.Fatalf("receipts %d, want 1", c)
	}
}

// 23514 check, 23001 restrict (append-only and immutability triggers).
var backstopCodes = map[string]bool{"23514": true, "23001": true}

// The database refuses at commit what the code must never do.
func TestReservationBackstops(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_bs", "r_bs")
	r, err := hold(st, ctx, "bs-1", "r_bs", f, time.Minute, item("ra", 4))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Reservations[0].ID
	for _, q := range []string{
		`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'ra'`,
		`UPDATE reservations SET amount = 1 WHERE id = '` + id + `'`,
		`UPDATE reservations SET fence = fence + 1 WHERE id = '` + id + `'`,
		`UPDATE reservations SET state = 'RELEASED', closed_at = now() WHERE id = '` + id + `'`,
		`DELETE FROM reservation_events WHERE reservation_id = '` + id + `'`,
		`DELETE FROM reservations WHERE id = '` + id + `'`,
		`INSERT INTO usage_receipts (resource, receipt_id, reservation_id, actual) VALUES ('ra', 'u-forged', '` + id + `', 1)`,
		`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('` + id + `', 2, 'RELEASED')`,
		`UPDATE resource_capacities SET capacity = 3 WHERE resource = 'ra'`,
	} {
		// Refused for the right reason: a constraint, never a broken trigger
		// (an undefined-column error would make this line vacuous).
		_, err := st.pool.Exec(ctx, q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !backstopCodes[pgErr.Code] {
			t.Errorf("not refused by a constraint (%v): %s", err, q)
		}
	}
	if h := held(t, st, ctx, "ra"); h != 4 {
		t.Fatalf("held %d after refused writes", h)
	}
	if res, err := st.Release(ctx, id); err != nil || !res.Changed {
		t.Fatalf("release after refused writes: %+v %v", res, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE reservations SET state = 'EXPIRED' WHERE id = '`+id+`'`); err == nil {
		t.Fatal("a closed reservation changed state again")
	}
	// Resurrection: a closed hold back to HELD with held fixed up in the same
	// transaction satisfies the sum and history checks; only "closed is final"
	// refuses it.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err1 := tx.Exec(ctx, `UPDATE reservations SET state = 'HELD', closed_at = NULL WHERE id = '`+id+`'`)
	_, err2 := tx.Exec(ctx, `UPDATE resource_capacities SET held = held + 4 WHERE resource = 'ra'`)
	err3 := tx.Commit(ctx)
	tx.Rollback(ctx) //nolint:errcheck
	if err1 == nil && err2 == nil && err3 == nil {
		t.Fatal("a released reservation was resurrected to HELD")
	}
	var pgErr *pgconn.PgError
	for _, e := range []error{err1, err2, err3} {
		if e != nil {
			if !errors.As(e, &pgErr) || !backstopCodes[pgErr.Code] {
				t.Fatalf("resurrection refused for the wrong reason: %v", e)
			}
			break
		}
	}
	if h := held(t, st, ctx, "ra"); h != 0 {
		t.Fatalf("held %d after a refused resurrection", h)
	}
}

// Red-team F5 on f54d526: a hold outlives its TTL while its attempt is live
// or unresolved (a runner may be running), and is closed by the sweep once
// the attempt has ended, due or not.
func TestHoldLivesAsLongAsItsAttempt(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_hl", "r_hl")
	if _, err := hold(st, ctx, "hl-1", "r_hl", f, time.Second, item("ra", 3)); err != nil {
		t.Fatal(err)
	}
	f2 := liveAttempt(t, st, ctx, "m_hl2", "r_hl2")
	if _, err := hold(st, ctx, "hl-2", "r_hl2", f2, time.Minute, item("ra", 2)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if n, err := st.ExpireEndedReservations(ctx, 10); err != nil || n != 0 {
		t.Fatalf("the sweep closed %d holds of live attempts (%v)", n, err)
	}
	if h := held(t, st, ctx, "ra"); h != 5 {
		t.Fatalf("held %d with both attempts live, want 5", h)
	}
	for _, x := range []struct {
		m domain.MissionID
		a domain.AttemptID
		f int64
	}{{"m_hl", "r_hl", f}, {"m_hl2", "r_hl2", f2}} {
		ok(t, st, ctx, "fabd", "fin-"+string(x.a), domain.Command{Kind: domain.CmdFinishAttempt, Mission: x.m,
			Attempt: x.a, Fence: x.f, Outcome: "abandoned"})
	}
	if n, err := st.ExpireEndedReservations(ctx, 10); err != nil || n != 2 {
		t.Fatalf("the sweep closed %d holds of ended attempts (%v), want 2 (one due, one not)", n, err)
	}
	if h := held(t, st, ctx, "ra"); h != 0 {
		t.Fatalf("held %d after both attempts ended, want 0", h)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE state = 'EXPIRED'`); c != 1 {
		t.Fatalf("%d EXPIRED, want 1 (the due one; the other is RELEASED)", c)
	}
}

// goodqa#15123 (red-team B4): a spend hold is never RELEASED when its
// attempt ends. A not-due one stays HELD (still counted against capacity)
// and is EXPIRED once due, so the spend ledger charges it; a not-due spend
// hold does not fill the sweep's limit ahead of other rows; other holds of
// an ended attempt are still released (control).
func TestSpendHoldNotReleasedOnEnd(t *testing.T) {
	st, ctx := openTest(t)
	const spendA, spendB = "spend.month.aa", "spend.run.bb"
	for _, r := range []string{spendA, spendB, "ra"} {
		capacity(t, st, ctx, r, 10)
	}
	// r_s1 sorts first, so under limit 1 its not-due spend hold would be the
	// only row the pre-fix sweep picks.
	f1 := liveAttempt(t, st, ctx, "m_s1", "r_s1")
	if _, err := hold(st, ctx, "s1", "r_s1", f1, time.Minute, item(spendA, 4)); err != nil {
		t.Fatal(err)
	}
	f2 := liveAttempt(t, st, ctx, "m_s2", "r_s2")
	if _, err := hold(st, ctx, "s2", "r_s2", f2, 5*time.Second, item(spendB, 3), item("ra", 2)); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		m domain.MissionID
		a domain.AttemptID
		f int64
	}{{"m_s1", "r_s1", f1}, {"m_s2", "r_s2", f2}} {
		ok(t, st, ctx, "fabd", "fin-"+string(x.a), domain.Command{Kind: domain.CmdFinishAttempt, Mission: x.m,
			Attempt: x.a, Fence: x.f, Outcome: "abandoned"})
	}
	if n, err := st.ExpireEndedReservations(ctx, 1); err != nil || n != 1 {
		t.Fatalf("limit-1 sweep closed %d (%v), want 1: the non-spend hold of r_s2", n, err)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE resource = 'ra' AND state = 'RELEASED'`); c != 1 {
		t.Fatalf("the non-spend hold of an ended attempt was not released (%d)", c)
	}
	if n, err := st.ExpireEndedReservations(ctx, 10); err != nil || n != 0 {
		t.Fatalf("sweep closed %d not-due spend holds (%v), want 0", n, err)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE starts_with(resource, 'spend.') AND state = 'HELD'`); c != 2 {
		t.Fatalf("%d spend holds HELD after their attempts ended, want 2 (none RELEASED)", c)
	}
	if h := held(t, st, ctx, spendA); h != 4 {
		t.Fatalf("%s held %d, want 4 (the hold still counts against capacity)", spendA, h)
	}
	// Wait, by DB time, until spendB is due (inspection finding 4: no fixed
	// sleep racing a short TTL).
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var due bool
		if err := st.pool.QueryRow(ctx, `SELECT expires_at <= clock_timestamp() FROM reservations WHERE resource = $1`, spendB).Scan(&due); err != nil {
			t.Fatal(err)
		}
		if due {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spendB never became due")
		}
	}
	if n, err := st.ExpireEndedReservations(ctx, 10); err != nil || n != 1 {
		t.Fatalf("sweep closed %d due spend holds (%v), want 1", n, err)
	}
	var state string
	if err := st.pool.QueryRow(ctx, `SELECT state FROM reservations WHERE resource = $1`, spendB).Scan(&state); err != nil || state != ResExpired {
		t.Fatalf("due spend hold of an ended attempt is %q (%v), want EXPIRED", state, err)
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE starts_with(resource, 'spend.') AND state = 'RELEASED'`); c != 0 {
		t.Fatalf("%d spend holds RELEASED", c)
	}
}

// goodqa#15124: the month key is database time in UTC.
func TestDBMonth(t *testing.T) {
	st, ctx := openTest(t)
	m, err := st.DBMonth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Against Go's UTC month of the DB clock, not the same SQL expression.
	var dbNow time.Time
	if err := st.pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	if want := dbNow.UTC().Format("200601"); m != want {
		t.Fatalf("DBMonth %q, want %q", m, want)
	}
}

// goodqa#15123 (inspection finding 2): the store itself refuses to RELEASE
// a spend hold, whoever asks, so the rule does not rest on the sweep alone.
// A non-spend hold releases as before (control).
func TestSpendHoldNeverReleased(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "spend.job.cc", 10)
	capacity(t, st, ctx, "ra", 10)
	f := liveAttempt(t, st, ctx, "m_sr", "r_sr")
	res, err := hold(st, ctx, "sr-1", "r_sr", f, time.Minute, item("spend.job.cc", 4), item("ra", 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Reservations {
		got, err := st.Release(ctx, r.ID)
		if r.Resource == "ra" {
			if err != nil || !got.Changed {
				t.Fatalf("release of a non-spend hold: %+v %v", got, err)
			}
			continue
		}
		if !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("release of a spend hold: %+v %v, want ErrInvalidTransition", got, err)
		}
	}
	if c := count(t, st, ctx, `SELECT count(*) FROM reservations WHERE resource = 'spend.job.cc' AND state = 'HELD'`); c != 1 {
		t.Fatalf("spend hold not HELD after a refused release (%d)", c)
	}
	if h := held(t, st, ctx, "spend.job.cc"); h != 4 {
		t.Fatalf("spend capacity held %d, want 4", h)
	}
}
