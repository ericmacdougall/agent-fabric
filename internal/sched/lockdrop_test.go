package sched

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15087 v9 risk: fabd's lock connection drops mid-tick, after the
// tick passed its lock check, and a `fab reconcile` takes the lock and acts
// first. The rest of fabd's tick then issues the same expire and reconcile
// commands under the same actor and ids: each is a replay, never a second
// transition. fabd's next tick sees the lost lock and re-takes it.
func TestReconcileLockDropReplays(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	s := New(st, Config{Host: hostA}, &fakeLauncher{st: st})
	s.prober = goneProber()
	s.RequireReconcileLock()
	defer s.Close()
	if _, err := s.Tick(ctx); err != nil || !s.HoldsReconcileLock() {
		t.Fatalf("first tick: %v, holds %v", err, s.HoldsReconcileLock())
	}
	lapsedAttempt(t, st, "m_rg", "r_rg", hostA)
	var leaseMillis, fence int64
	if err := st.Pool().QueryRow(ctx, `SELECT (extract(epoch FROM lease_until) * 1000)::bigint, fence FROM attempts WHERE id = 'r_rg'`).Scan(&leaseMillis, &fence); err != nil {
		t.Fatal(err)
	}
	// Drop fabd's lock session from the server side. pg_locks shows the
	// two int4 keys of a two-key advisory lock as classid and objid, with
	// objsubid 2.
	k1, k2 := store.ReconcileLockKeys(hostA)
	var killed bool
	if err := st.Pool().QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 2 AND granted`,
		int64(uint32(k1)), int64(uint32(k2))).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the lock session: %v (killed %v)", err, killed)
	}
	// pg_terminate_backend only queues the termination: the backend releases
	// its advisory lock as it dies, asynchronously. Reconciling before the
	// lock row disappears would report "another reconciler holds", so wait
	// for the release (bounded) and fail if it never comes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var held bool
		if err := st.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 2 AND granted)`,
			int64(uint32(k1)), int64(uint32(k2))).Scan(&held); err != nil {
			t.Fatalf("watching the lock release: %v", err)
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminated backend still holds the reconcile lock after 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, err := ReconcileHost(ctx, st, hostA, hostA, goneProber(), nil)
	if err != nil || len(r.Expired) != 1 || r.Quarantined != 1 {
		t.Fatalf("fab reconcile after the drop: %+v %v, want 1 expired, 1 quarantined", r, err)
	}
	cmds, evs := rowCount(t, st, "commands"), rowCount(t, st, "events")
	// The remainder of fabd's tick, which had already selected r_rg.
	for _, c := range []struct {
		id  domain.CommandID
		cmd domain.Command
	}{
		{domain.CommandID(fmt.Sprintf("expire:f%d:%d", fence, leaseMillis)), domain.Command{Kind: domain.CmdExpireLease, Mission: "m_rg", Attempt: "r_rg", Fence: fence}},
		{"reconcile:gone:r_rg", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_rg", Attempt: "r_rg", Fence: fence,
			Result: "QUARANTINED", Reason: "runner gone after its lease lapsed; outcome unverified"}},
	} {
		resp, err := st.Execute(ctx, "fabd", c.id, c.cmd)
		if err != nil || !resp.Replayed {
			t.Fatalf("fabd's %s after fab reconcile: replayed %v, %v; want a replay", c.id, resp.Replayed, err)
		}
	}
	if c, e := rowCount(t, st, "commands"), rowCount(t, st, "events"); c != cmds || e != evs {
		t.Fatalf("the overlap wrote: commands %d -> %d, events %d -> %d", cmds, c, evs, e)
	}
	if got := attemptState(t, st, "r_rg"); got != string(domain.AttemptQuarantined) {
		t.Fatalf("r_rg is %s, want QUARANTINED", got)
	}
	// fabd's next tick notices the dead session and re-takes the free lock.
	if _, err := s.Tick(ctx); err != nil || !s.HoldsReconcileLock() {
		t.Fatalf("tick after the drop: %v, holds %v; want the lock re-taken", err, s.HoldsReconcileLock())
	}
	if _, err := ReconcileHost(ctx, st, hostA, hostA, goneProber(), nil); !errors.Is(err, store.ErrReconcileBusy) {
		t.Fatalf("fab reconcile after fabd re-took the lock: %v, want ErrReconcileBusy", err)
	}
}
