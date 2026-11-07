package main

import (
	"errors"
	"strings"
	"testing"

	"fabric/internal/hostid"
	"fabric/internal/sched"
)

// goodqa#15087 assumption A4: `fab reconcile` acts only on the host it runs
// on. Another --host, or a stray argument, is refused with exit 3 before
// the store is opened (the DSN below would fail to connect if it were),
// naming both hosts instead of guessing. The lock and host-filter cases
// are internal/sched TestReconcileExclusive.
func TestReconcileCommand(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://refused.invalid/none") // never reached
	local := string(hostid.Default())
	err := reconcileMain([]string{"--host", "h_elsewhere"})
	if code := exitCode(err); code != 3 || !errors.Is(err, sched.ErrWrongHost) ||
		!strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), "h_elsewhere") {
		t.Fatalf("reconcile --host h_elsewhere: exit %d (%v), want 3, ErrWrongHost naming %s and h_elsewhere", code, err, local)
	}
	if err := reconcileMain([]string{"extra"}); exitCode(err) != 3 {
		t.Fatalf("reconcile extra: exit %d (%v), want 3", exitCode(err), err)
	}
	// Positive control: this host gets past the host check to the store,
	// which the invalid DSN refuses (exit 1, not a refusal).
	if err := reconcileMain([]string{"--host", local}); err == nil || exitCode(err) == 3 || errors.Is(err, sched.ErrWrongHost) {
		t.Fatalf("reconcile --host %s: exit %d (%v), want a store error", local, exitCode(err), err)
	}
}
