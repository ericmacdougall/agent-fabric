package sched

import (
	"context"
	"testing"

	"fabric/internal/domain"
)

// The attempt budget: consecutive pre-launch failures attributable to the
// mission (here its launch is refused) park it as BLOCKED with a reason.
func TestPrelaunchBudgetParksMission(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_hl3")
	cfg.Launch.Binary = "" // every launch is refused before ACK
	ctx := context.Background()
	s := New(st, cfg, ln)
	for i := 0; i < 3*domain.PrelaunchBudget; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		// Let the window pass without waiting for it.
		if _, err := st.Pool().Exec(ctx, "UPDATE missions SET not_before = NULL WHERE id = 'm_hl3'"); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := counts(t, st, "m_hl3")
	m, err := st.Mission(ctx, "m_hl3")
	if err != nil {
		t.Fatal(err)
	}
	if a != domain.PrelaunchBudget || m.Execution != domain.ExecBlocked || m.ParkedReason == "" {
		t.Fatalf("after %d pre-launch failures: %d attempts, execution %s, reason %q; want %d, BLOCKED, a reason",
			3*domain.PrelaunchBudget, a, m.Execution, m.ParkedReason, domain.PrelaunchBudget)
	}
}

// Red-team D2: a full resource is backpressure, not a failure. The mission
// backs off (capped) and never parks, however long it waits.
func TestCapacityWaitNeverParks(t *testing.T) {
	st, ln, cfg := gatedSetup(t, "m_cw", 0)
	ctx := context.Background()
	s := New(st, cfg, ln)
	n := 3 * domain.PrelaunchBudget
	for i := 0; i < n; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, "UPDATE missions SET not_before = NULL WHERE id = 'm_cw'"); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := counts(t, st, "m_cw")
	m, err := st.Mission(ctx, "m_cw")
	if err != nil {
		t.Fatal(err)
	}
	if a != n || m.Execution == domain.ExecBlocked || m.PrelaunchFailures != 0 || m.DeferMS != domain.PrelaunchMaxMS {
		t.Fatalf("after %d capacity waits: %d attempts, execution %s, failures %d, backoff %d; want %d, not BLOCKED, 0, the cap",
			n, a, m.Execution, m.PrelaunchFailures, m.DeferMS, n)
	}
}

// Nit: re-admission clears the backoff column as well as the count.
func TestReadmissionClearsNotBefore(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_ra")
	cfg.Launch.Binary = ""
	ctx := context.Background()
	s := New(st, cfg, ln)
	for i := 0; i < domain.PrelaunchBudget; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if i < domain.PrelaunchBudget-1 {
			if _, err := st.Pool().Exec(ctx, "UPDATE missions SET not_before = now() WHERE id = 'm_ra'"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The last deferral before the park left not_before set; force one in the future.
	if _, err := st.Pool().Exec(ctx, "UPDATE missions SET not_before = now() + interval '5 minutes' WHERE id = 'm_ra'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "readmit-m_ra", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ra"}); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err := st.Pool().QueryRow(ctx, "SELECT not_before IS NULL FROM missions WHERE id = 'm_ra'").Scan(&cleared); err != nil || !cleared {
		t.Fatalf("re-admission left not_before set (%v)", err)
	}
}
