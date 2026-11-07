package sched

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// C5 (0012), ported by intent from Kimi's dispatch-gate tests: with holds
// configured, the runner starts only on a stored grant decided after
// ACK_START; every denial, including a hard stop at the after-admission
// cutpoint, makes zero Launcher.Start calls.

func gatedSetup(t *testing.T, id domain.MissionID, capacity int64) (*store.Store, *fakeLauncher, Config) {
	t.Helper()
	st, ln, cfg := ownerSetup(t, id)
	if err := st.SetCapacity(context.Background(), "slot.h_t2", "slots", capacity); err != nil {
		t.Fatal(err)
	}
	cfg.Holds = []store.HoldItem{{Resource: "slot.h_t2", Unit: "slots", Amount: 1}}
	cfg.HoldTTL = time.Minute
	return st, ln, cfg
}

func attemptState(t *testing.T, st *store.Store, a string) string {
	t.Helper()
	var s string
	if err := st.Pool().QueryRow(context.Background(), "SELECT state FROM attempts WHERE id = $1", a).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func grantOutcome(t *testing.T, st *store.Store, a string) string {
	t.Helper()
	var o, r string
	err := st.Pool().QueryRow(context.Background(), "SELECT outcome, reason FROM dispatch_grants WHERE attempt_id = $1", a).Scan(&o, &r)
	if err != nil {
		return "none"
	}
	return o + ": " + r
}

func heldNow(t *testing.T, st *store.Store) int64 {
	t.Helper()
	_, h, err := st.Capacity(context.Background(), "slot.h_t2")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDispatchGateGrantStartsExactlyOnce(t *testing.T) {
	st, ln, cfg := gatedSetup(t, "m_g1", 1)
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_g1"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&ln.starts); n != 1 {
		t.Fatalf("%d starts, want 1", n)
	}
	if g := grantOutcome(t, st, "r_g1_1"); g != "GRANTED: " {
		t.Fatalf("grant %q", g)
	}
	if h := heldNow(t, st); h != 1 {
		t.Fatalf("held %d while running, want 1", h)
	}
}

func TestDispatchGateDenialsNeverStart(t *testing.T) {
	cases := []struct {
		name     string
		capacity int64
		ttl      time.Duration
		cut      func(t *testing.T, st *store.Store, id domain.MissionID) // after ACK, before the grant
		want     string
	}{
		{"hold refused at capacity", 0, time.Minute, nil, "none"},
		{"intent revised after admission", 1, time.Minute, func(t *testing.T, st *store.Store, id domain.MissionID) {
			if _, err := st.Execute(context.Background(), "chat", domain.CommandID("rev-"+string(id)), domain.Command{Kind: domain.CmdReviseIntent, Mission: id, Reason: "scope"}); err != nil {
				t.Fatal(err)
			}
		}, "DENIED: intent revised"},
		{"hold released after admission", 1, time.Minute, func(t *testing.T, st *store.Store, id domain.MissionID) {
			rs, _ := st.Reservations(context.Background(), domain.AttemptID("r_"+strings.TrimPrefix(string(id), "m_")+"_1"))
			if _, err := st.Release(context.Background(), rs[0].ID); err != nil {
				t.Fatal(err)
			}
		}, "DENIED: no held reservation"},
		{"hold expired by DB time after admission", 1, store.MinHoldTTL, func(t *testing.T, st *store.Store, id domain.MissionID) {
			rs, _ := st.Reservations(context.Background(), domain.AttemptID("r_"+strings.TrimPrefix(string(id), "m_")+"_1"))
			for {
				var past bool
				if err := st.Pool().QueryRow(context.Background(), "SELECT clock_timestamp() > $1", rs[0].ExpiresAt).Scan(&past); err != nil || past {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}, "DENIED: reservation for slot.h_t2 expired"},
	}
	for i, c := range cases {
		id := domain.MissionID("m_gd" + string(rune('1'+i)))
		a := "r_gd" + string(rune('1'+i)) + "_1"
		st, ln, cfg := gatedSetup(t, id, c.capacity)
		cfg.HoldTTL = c.ttl
		s := New(st, cfg, ln)
		if c.cut != nil {
			s.afterAck = func() { c.cut(t, st, id) }
		}
		if err := s.runOnce(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Errorf("%s: %d starts, want 0", c.name, n)
		}
		if g := grantOutcome(t, st, a); !strings.HasPrefix(g, c.want) {
			t.Errorf("%s: grant %q, want %q", c.name, g, c.want)
		}
		if s := attemptState(t, st, a); s != string(domain.AttemptFailedDefinite) {
			t.Errorf("%s: attempt %s, want FAILED_DEFINITE", c.name, s)
		}
		if h := heldNow(t, st); h != 0 {
			t.Errorf("%s: held %d after the denial, want 0", c.name, h)
		}
	}
}

func TestDispatchGateOffTouchesNothing(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_g0")
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_g0"); err != nil {
		t.Fatal(err)
	}
	var grants, res int
	st.Pool().QueryRow(context.Background(), "SELECT (SELECT count(*) FROM dispatch_grants), (SELECT count(*) FROM reservations)").Scan(&grants, &res) //nolint:errcheck
	if atomic.LoadInt32(&ln.starts) != 1 || grants != 0 || res != 0 {
		t.Fatalf("gate off: starts %d, grants %d, reservations %d; want 1, 0, 0", ln.starts, grants, res)
	}
}

func TestTickSweepsReservationsOnlyWhenGated(t *testing.T) {
	st, ln, cfg := gatedSetup(t, "m_gs", 1)
	cfg.HoldTTL = store.MinHoldTTL
	gated := New(st, cfg, ln)
	if err := gated.runOnce(context.Background(), "m_gs"); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.Reservations(context.Background(), "r_gs_1")
	for {
		var past bool
		if err := st.Pool().QueryRow(context.Background(), "SELECT clock_timestamp() > $1", rs[0].ExpiresAt).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	off := cfg
	off.Holds = nil
	if _, err := New(st, off, ln).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := heldNow(t, st); h != 1 {
		t.Fatalf("an ungated tick swept reservations: held %d", h)
	}
	if _, err := gated.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := heldNow(t, st); h != 0 {
		t.Fatalf("gated tick left held %d, want 0", h)
	}
}
