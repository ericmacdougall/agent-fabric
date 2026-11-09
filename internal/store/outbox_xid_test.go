package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// goodqa#15577. The consumer's visibility filter is
// `o.xid < pg_snapshot_xmin(pg_current_snapshot())` and the cursor moves to the
// largest id read. That is safe only if outbox ids are assigned in the same
// order as transaction ids. Before the fix Execute took its xid at the mission
// row lock and its outbox id later, so two writers on different missions could
// lock in one order and insert in the other:
//
//	T1 locks mission A (xid a)    T2 locks mission B (xid b > a)
//	T2 inserts its outbox row (id 10, xid b)    T1 inserts (id 11, xid a)
//	T1 commits while T2 is open: xmin = b, row 11 (a < b) is visible, row 10 is
//	not; the cursor moves to 11; row 10 is never read.
//
// Execute now takes a session advisory lock before anything assigns an xid and
// releases it right after the last outbox insert (before the commit).

func waitUntil(t *testing.T, what string, d time.Duration, f func() bool) bool {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if f() {
			return true
		}
	}
	return false
}

// advisoryHeld counts granted session or transaction locks on the order key.
func advisoryHeld(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND ((classid::bigint << 32) | objid::bigint) = $1`, int64(outboxOrderLock)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitingWithoutXid counts backends waiting for the order lock that have not
// been assigned a transaction id yet.
func waitingWithoutXid(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND NOT l.granted AND a.backend_xid IS NULL
		AND ((l.classid::bigint << 32) | l.objid::bigint) = $1`, int64(outboxOrderLock)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The deterministic interleave, through Execute. Writer A (mission m_xa) is
// parked right after its mission row lock, before any outbox insert. Writer B
// (mission m_xb) starts and, in a build without the fix, runs on to its commit
// hook and parks there with the LOWER outbox id and the HIGHER xid; A is then
// released, inserts the higher id and commits first; the consumer reads A's
// row only and its cursor passes B's id, so B's row is never delivered. With
// the fix B waits on the advisory lock with no xid until A has inserted.
func TestOutboxInterleaveThroughExecute(t *testing.T) {
	st, ctx := openTest(t)
	for _, m := range []domain.MissionID{"m_xa", "m_xb"} {
		mk(t, st, ctx, m)
	}
	const consumer = "xidtest"
	if _, err := st.ConsumeOutbox(ctx, consumer, 1000, func(context.Context, pgx.Tx, OutboxRow) error { return nil }); err != nil {
		t.Fatal(err)
	}
	inA, relA := make(chan struct{}), make(chan struct{})
	inB, relB := make(chan struct{}), make(chan struct{})
	var onceA, onceB, onceRA, onceRB sync.Once
	unblockA := func() { onceRA.Do(func() { close(relA) }) }
	unblockB := func() { onceRB.Do(func() { close(relB) }) }
	t.Cleanup(unblockA)
	t.Cleanup(unblockB)
	st.hooks.afterMissionLock = func(cmd domain.Command) {
		if cmd.Mission == "m_xa" {
			onceA.Do(func() { close(inA) })
			<-relA
		}
	}
	st.hooks.beforeCommit = func(cmd domain.Command) error {
		if cmd.Mission == "m_xb" {
			onceB.Do(func() { close(inB) })
			<-relB
		}
		return nil
	}
	t.Cleanup(func() { st.hooks.afterMissionLock, st.hooks.beforeCommit = nil, nil })

	var wg sync.WaitGroup
	run := func(m domain.MissionID, key string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := domain.Command{Kind: domain.CmdReviseIntent, Mission: m, Reason: "x"}
			if _, err := st.Execute(ctx, "chat", domain.CommandID(key), cmd); err != nil {
				t.Errorf("%s: %v", m, err)
			}
		}()
	}
	run("m_xa", "xi-a")
	select {
	case <-inA:
	case <-time.After(10 * time.Second):
		t.Fatal("writer A never reached the hook")
	}
	run("m_xb", "xi-b")
	// With the fix B is queued on the advisory lock and has no xid. Without it
	// B reaches its commit hook (it already holds the lower outbox id).
	queued := waitUntil(t, "B queued", 3*time.Second, func() bool { return waitingWithoutXid(t, st, ctx) == 1 })
	reachedHook := false
	select {
	case <-inB:
		reachedHook = true
	default:
	}
	if !queued || reachedHook {
		unblockA()
		unblockB()
		wg.Wait()
		t.Fatalf("writer B was not queued on the order lock while A held it (queued=%v, B reached its commit hook=%v): B can take its xid and a lower outbox id ahead of A (goodqa#15577)", queued, reachedHook)
	}
	// Release A; B then proceeds and parks at its commit hook; A has committed.
	unblockA()
	select {
	case <-inB:
	case <-time.After(10 * time.Second):
		t.Fatal("writer B never proceeded after A released the lock")
	}
	seen := map[int64]int{}
	apply := func(_ context.Context, _ pgx.Tx, r OutboxRow) error {
		if r.Mission == "m_xa" || r.Mission == "m_xb" {
			seen[r.ID]++
		}
		return nil
	}
	if _, err := st.ConsumeOutbox(ctx, consumer, 1000, apply); err != nil {
		t.Fatal(err)
	}
	unblockB()
	wg.Wait()
	for i := 0; i < 3; i++ {
		if _, err := st.ConsumeOutbox(ctx, consumer, 1000, apply); err != nil {
			t.Fatal(err)
		}
	}
	var committed int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE mission_id IN ('m_xa','m_xb') AND kind = 'INTENT_REVISED'`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 2 || len(seen) != 2 {
		t.Fatalf("committed %d rows, delivered %d (%v)", committed, len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %d delivered %d times", id, n)
		}
	}
}

// After Execute returns, on every path, the order lock is not held by any
// session (a leaked session lock would block every later writer).
func TestOutboxOrderLockNeverLeaks(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_lk")
	check := func(what string) {
		t.Helper()
		if n := advisoryHeld(t, st, ctx); n != 0 {
			t.Fatalf("%s: the order lock is still held (%d granted locks)", what, n)
		}
	}
	check("before")
	admit := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_lk"}
	if _, err := st.Execute(ctx, "chat", "lk-admit", admit); err != nil {
		t.Fatal(err)
	}
	check("ok path")
	if _, err := st.Execute(ctx, "chat", "lk-admit", admit); err != nil {
		t.Fatalf("replay: %v", err)
	}
	check("replay path")
	if _, err := st.Execute(ctx, "chat", "lk-admit2", admit); err == nil {
		t.Fatal("a second admit should be refused")
	}
	check("refusal path")
	if _, err := st.Execute(ctx, "chat", "lk-none", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_nosuch"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown mission: %v", err)
	}
	check("not-found path")
	holdIt(t, st, ctx, "m_lk")
	if _, err := st.Execute(ctx, "chat", "lk-held", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_lk", Reason: "x"}); !errors.Is(err, domain.ErrStreamHeld) {
		t.Fatalf("held: %v", err)
	}
	check("held path")
	plantBad(t, st, ctx, "m_lk", 900, "NEW_FUTURE_KIND", `{"kind":"NEW_FUTURE_KIND","mission":"m_lk","seq":900}`)
	if _, err := st.Execute(ctx, "chat", "lk-foreign", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_lk", Reason: "x"}); !errors.Is(err, domain.ErrUnsupportedEvent) {
		t.Fatalf("foreign kind: %v", err)
	}
	check("unsupported path")
	// A cancelled context, before and while waiting for the lock.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.Execute(cctx, "chat", "lk-cancel", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_lk", Reason: "x"}); err == nil {
		t.Fatal("a cancelled context should fail")
	}
	check("cancelled path")
	// A failure injected after the outbox insert (the commit hook).
	st.hooks.beforeCommit = func(domain.Command) error { return fmt.Errorf("injected") }
	t.Cleanup(func() { st.hooks.beforeCommit = nil })
	mk(t, st, ctx, "m_lk2")
	if _, err := st.Execute(ctx, "chat", "lk-inject", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_lk2", Reason: "x"}); err == nil {
		t.Fatal("the injected failure should abort")
	}
	check("aborted-before-commit path")
}

// The lock wait while another writer holds the lock is cancellable and leaves
// no lock behind.
func TestOutboxOrderLockWaitCancelled(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_cw0")
	holder, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", outboxOrderLock); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = st.Execute(wctx, "chat", "cw-wait", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cw0"})
	if err == nil {
		t.Fatal("Execute finished although the order lock is held elsewhere")
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", outboxOrderLock); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(t, "lock free", 3*time.Second, func() bool { return advisoryHeld(t, st, ctx) == 0 }) {
		t.Fatal("the cancelled writer left the order lock held")
	}
	if _, err := st.Execute(ctx, "chat", "cw-after", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cw0"}); err != nil {
		t.Fatalf("a later writer: %v", err)
	}
}

func TestOutboxNoRowLostUnderConcurrentWriters(t *testing.T) {
	st, ctx := openTest(t)
	const missions, perMission = 4, 40 // the slot cap is 4
	ids := make([]domain.MissionID, missions)
	for i := range ids {
		ids[i] = domain.MissionID(fmt.Sprintf("m_cw%d", i+1))
		mk(t, st, ctx, ids[i])
		ok(t, st, ctx, "chat", fmt.Sprintf("cw-admit-%d", i), domain.Command{Kind: domain.CmdAdmitMission, Mission: ids[i]})
	}
	const consumer = "cwtest"
	seen := map[int64]int{}
	var mu sync.Mutex
	apply := func(_ context.Context, _ pgx.Tx, r OutboxRow) error {
		mu.Lock()
		seen[r.ID]++
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	consumed := make(chan struct{})
	go func() { // the consumer runs the whole time, as the scheduler does
		defer close(consumed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := st.ConsumeOutbox(ctx, consumer, 50, apply); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i, m := range ids {
		wg.Add(1)
		go func(i int, m domain.MissionID) {
			defer wg.Done()
			for j := 0; j < perMission; j++ {
				if _, err := st.Execute(ctx, "chat", domain.CommandID(fmt.Sprintf("cw-rev-%d-%d", i, j)), domain.Command{Kind: domain.CmdReviseIntent, Mission: m, Reason: fmt.Sprintf("r%d", j)}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i, m)
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-consumed
	for i := 0; i < 5; i++ {
		if _, err := st.ConsumeOutbox(ctx, consumer, 1000, apply); err != nil {
			t.Fatal(err)
		}
	}
	var total, delivered, dup int
	rows, err := st.pool.Query(ctx, `SELECT id FROM outbox WHERE mission_id LIKE 'm_cw_' AND kind = 'INTENT_REVISED'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		total++
		switch seen[id] {
		case 0:
		case 1:
			delivered++
		default:
			dup++
		}
	}
	if total != missions*perMission || delivered != total || dup != 0 {
		t.Fatalf("revise rows %d, delivered once %d, more than once %d (goodqa#15577)", total, delivered, dup)
	}
	if n := advisoryHeld(t, st, ctx); n != 0 {
		t.Fatalf("%d order locks still held after the writers finished", n)
	}
	// The invariant the consumer's filter relies on, read straight from the
	// table: no pair of outbox rows has a lower id with a higher xid.
	var inversions int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox a JOIN outbox b ON a.id < b.id AND a.xid > b.xid`).Scan(&inversions); err != nil {
		t.Fatal(err)
	}
	if inversions != 0 {
		t.Fatalf("%d outbox row pairs have a lower id with a higher xid (goodqa#15577)", inversions)
	}
}
