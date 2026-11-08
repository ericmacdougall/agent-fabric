package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/domain"
)

// Every fenced loop write refuses an attempt whose lease has expired or was
// never set: the sweep may already have reclaimed it.
func TestFencedWriteRefusesExpiredLease(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, set string
		ok        bool
	}{
		{"live lease", "now() + interval '1 hour'", true},
		{"expired lease", "now() - interval '1 second'", false},
		{"no lease", "NULL", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			if _, err := r.st.Pool().Exec(ctx, `UPDATE attempts SET lease_until = `+c.set+` WHERE id = $1`, string(attemptID)); err != nil {
				t.Fatal(err)
			}
			_, err := r.db.StartRun(ctx, attemptID, fence, agent.BillingAPIKey, "cfg", 3)
			if c.ok {
				if err != nil {
					t.Fatalf("StartRun with a live lease: %v", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidTransition) || err == nil || !strings.Contains(err.Error(), "lease has expired") {
				t.Fatalf("StartRun with %s = %v, want lease-expired ErrInvalidTransition", c.name, err)
			}
			var n int
			if err := r.st.Pool().QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE attempt_id = $1`, string(attemptID)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("refused StartRun still wrote %d agent_runs rows", n)
			}
		})
	}
}
