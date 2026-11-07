package sched

import (
	"context"
	"testing"
	"time"

	"fabric/internal/domain"
)

// The attempt budget: consecutive pre-launch failures park the mission as
// BLOCKED with a reason, whatever the backoff.
func TestPrelaunchBudgetParksMission(t *testing.T) {
	st, ln, cfg := gatedSetup(t, "m_hl3", 0)
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
	_ = time.Second
}
