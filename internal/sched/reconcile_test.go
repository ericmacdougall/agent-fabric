package sched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15087 NEG-14 (AC-7): the host reconcile lock. fabd holds it for
// its whole life, so `fab reconcile` (ReconcileHost, the code the command
// runs) is refused while fabd runs, whatever the tick timing; fabd does no
// reconcile work while another holder has it; and a pass for host H acts on
// H's attempts only.

const (
	hostA domain.HostID = "h_neg14a"
	hostB domain.HostID = "h_neg14b"
)

// lapsedAttempt starts a claude-code attempt for mission m on host h through
// the ordinary commands, then lets its lease lapse by database time.
func lapsedAttempt(t *testing.T, st *store.Store, m domain.MissionID, a domain.AttemptID, h domain.HostID) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#15087", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(m)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start-"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a,
		Host: h, Role: "writer", Path: domain.ProviderClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack-"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: resp.Events[0].Fence}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1", string(a)); err != nil {
		t.Fatal(err)
	}
}

func rowCount(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func goneProber() *fakeProber { return &fakeProber{alive: false, known: true} }

func TestReconcileExclusive(t *testing.T) {
	ctx := context.Background()

	t.Run("a a started scheduler holds the lock between ticks", func(t *testing.T) {
		st := storetest.Open(t)
		lapsedAttempt(t, st, "m_ra", "r_ra", hostA)
		s := New(st, Config{Host: hostA}, &fakeLauncher{st: st})
		s.RequireReconcileLock()
		defer s.Close()
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if !s.HoldsReconcileLock() {
			t.Fatal("the scheduler does not hold its host lock after a tick")
		}
		// Between ticks: nothing of the scheduler is running now.
		lapsedAttempt(t, st, "m_rb", "r_rb", hostA)
		cmds, evs := rowCount(t, st, "commands"), rowCount(t, st, "events")
		r, err := ReconcileHost(ctx, st, hostA, hostA, goneProber(), nil)
		if !errors.Is(err, store.ErrReconcileBusy) || !strings.Contains(err.Error(), string(hostA)) {
			t.Fatalf("reconcile while the scheduler holds the lock: %v, want ErrReconcileBusy naming %s", err, hostA)
		}
		if len(r.Expired) != 0 || r.Quarantined != 0 {
			t.Fatalf("refused pass reported work: %+v", r)
		}
		if c, e := rowCount(t, st, "commands"), rowCount(t, st, "events"); c != cmds || e != evs {
			t.Fatalf("refused pass wrote: commands %d -> %d, events %d -> %d", cmds, c, evs, e)
		}
		if got := attemptState(t, st, "r_rb"); got != string(domain.AttemptRunning) {
			t.Fatalf("r_rb is %s after the refused pass, want RUNNING", got)
		}
		s.Close()
		if _, err := ReconcileHost(ctx, st, hostA, hostA, goneProber(), nil); err != nil {
			t.Fatalf("reconcile after the scheduler let go: %v", err)
		}
	})

	t.Run("b an external holder leaves the scheduler idle for 3 ticks", func(t *testing.T) {
		st := storetest.Open(t)
		lapsedAttempt(t, st, "m_rc", "r_rc", hostA)
		ext, err := st.TryReconcileLock(ctx, hostA)
		if err != nil {
			t.Fatal(err)
		}
		s := New(st, Config{Host: hostA}, &fakeLauncher{st: st})
		s.prober = goneProber()
		s.RequireReconcileLock()
		defer s.Close()
		for i := 0; i < 3; i++ {
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if got := attemptState(t, st, "r_rc"); got != string(domain.AttemptRunning) || s.HoldsReconcileLock() || s.Expired != 0 {
				t.Fatalf("tick %d with the lock held elsewhere: r_rc %s, holds %v, expired %d; want RUNNING, false, 0", i+1, got, s.HoldsReconcileLock(), s.Expired)
			}
		}
		ext.Release()
		if _, err := s.Tick(ctx); err != nil { // positive control: the case is not vacuous
			t.Fatal(err)
		}
		if got := attemptState(t, st, "r_rc"); !s.HoldsReconcileLock() || got == string(domain.AttemptRunning) {
			t.Fatalf("after the release: holds %v, r_rc %s; want the lock and the lease expired", s.HoldsReconcileLock(), got)
		}
	})

	t.Run("c a pass for host A leaves host B's expired attempt RUNNING", func(t *testing.T) {
		st := storetest.Open(t)
		lapsedAttempt(t, st, "m_rd", "r_rd", hostA)
		lapsedAttempt(t, st, "m_re", "r_re", hostB)
		r, err := ReconcileHost(ctx, st, hostA, hostA, goneProber(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := attemptState(t, st, "r_re"); got != string(domain.AttemptRunning) {
			t.Fatalf("host B's attempt is %s after host A's pass, want RUNNING", got)
		}
		if got := attemptState(t, st, "r_rd"); got != string(domain.AttemptQuarantined) || len(r.Expired) != 1 || r.Quarantined != 1 {
			t.Fatalf("host A's attempt is %s (%+v), want QUARANTINED, 1 expired, 1 quarantined", got, r)
		}
	})

	t.Run("d another host is refused before the database", func(t *testing.T) {
		st := storetest.Open(t)
		lapsedAttempt(t, st, "m_rf", "r_rf", hostB)
		cmds := rowCount(t, st, "commands")
		_, err := ReconcileHost(ctx, st, hostB, hostA, goneProber(), nil)
		if !errors.Is(err, ErrWrongHost) || !strings.Contains(err.Error(), string(hostA)) || !strings.Contains(err.Error(), string(hostB)) {
			t.Fatalf("reconcile of %s from %s: %v, want ErrWrongHost naming both", hostB, hostA, err)
		}
		if c := rowCount(t, st, "commands"); c != cmds || attemptState(t, st, "r_rf") != string(domain.AttemptRunning) {
			t.Fatalf("refused pass wrote: commands %d -> %d", cmds, c)
		}
	})

	t.Run("e a single-key lock on the same numbers does not block it", func(t *testing.T) {
		st := storetest.Open(t)
		k1, k2 := store.ReconcileLockKeys(hostA)
		conn, err := st.Pool().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock_all()") //nolint:errcheck
		packed := int64(k1)<<32 | int64(uint32(k2))
		for _, key := range []int64{packed, int64(k1), int64(k2)} {
			if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1::bigint)", key); err != nil {
				t.Fatal(err)
			}
		}
		l, err := st.TryReconcileLock(ctx, hostA)
		if err != nil {
			t.Fatalf("two-key lock with single-key locks on the same numbers held: %v", err)
		}
		defer l.Release()
		if _, err := st.TryReconcileLock(ctx, hostA); !errors.Is(err, store.ErrReconcileBusy) {
			t.Fatalf("second two-key lock: %v, want ErrReconcileBusy", err) // positive control
		}
	})
}
