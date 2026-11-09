package store

import (
	"context"
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
// The paper interleaving was reproduced with two raw transactions on t2-dev
// (committed 2 rows, the consumer saw 1). Execute now takes a transaction
// advisory lock before anything assigns an xid, so the interleaving cannot
// happen through the writer. TestOutboxOrderLockBlocksSecondWriter proves a
// second writer cannot take its xid while a first is open; the stress test
// proves no row is lost under concurrent writers on different missions.
func TestOutboxOrderLockBlocksSecondWriter(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_ol1")
	mk(t, st, ctx, "m_ol2")
	// A raw transaction plays the first writer between its lock and its commit.
	t1, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Rollback(context.Background()) //nolint:errcheck
	if _, err := t1.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", outboxOrderLock); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.Execute(ctx, "chat", "ol-admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ol2"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a second writer finished while the first held the order lock (err %v): it could take its xid out of order", err)
	case <-time.After(400 * time.Millisecond):
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer never finished after the first committed")
	}
}

func TestOutboxNoRowLostUnderConcurrentWriters(t *testing.T) {
	st, ctx := openTest(t)
	const missions, perMission = 4, 40 // the slot cap is 4
	ids := make([]domain.MissionID, missions)
	for i := range ids {
		ids[i] = domain.MissionID(fmt.Sprintf("m_cw%d", i))
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
	// Let every writer's transaction leave the snapshot, then drain.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-consumed
	for i := 0; i < 5; i++ {
		if _, err := st.ConsumeOutbox(ctx, consumer, 1000, apply); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE mission_id LIKE 'm_cw%'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	var delivered, dup int
	rows, err := st.pool.Query(ctx, `SELECT id FROM outbox WHERE mission_id LIKE 'm_cw%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		switch seen[id] {
		case 0:
		case 1:
			delivered++
		default:
			dup++
		}
	}
	if total < missions*perMission || delivered != total || dup != 0 {
		t.Fatalf("outbox rows %d, delivered once %d, delivered more than once %d: a committed row was lost or repeated (goodqa#15577)", total, delivered, dup)
	}
}
