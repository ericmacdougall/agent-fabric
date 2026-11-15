package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15966 AC-18, crash-point finding F2 arm (a): a launch refused
// after the binding record is written and before ACK (here the pinned
// claude does not match, so ClaudeEnv refuses) settles the binding before
// its prelaunch FINISH, with no crash involved; before the fix the binding
// stayed unsettled forever and pinned its token version.
func TestM1RefusalAfterBindingSettlesIt(t *testing.T) {
	const m = domain.MissionID("m_ub")
	st, ln, cfg, bindings := tokenRig(t, m)
	cfg.Credential.PinnedSHA = strings.Repeat("0", 64)
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	const a = "r_ub_1"
	if _, err := os.Lstat(filepath.Join(bindings, a+".json")); err != nil {
		t.Fatal("fixture: no binding record was written, so the refusal came before it")
	}
	if _, err := os.Lstat(filepath.Join(bindings, a+".settled")); err != nil {
		t.Fatal("the refused launch left its binding unsettled")
	}
	if state, _ := m1AttemptState(t, st, a); state == string(domain.AttemptLeased) {
		t.Fatalf("attempt %s, want finished in prelaunch", state)
	}
	if n := atomic.LoadInt32(&ln.starts); n != 0 {
		t.Fatalf("%d processes started", n)
	}
}
