package sched

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
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

// F2, first half on its own: abandonPrelaunch settles an owed binding
// before its FINISH. The check runs the moment the FINISH has committed
// (afterFinishExec), so a settle that follows the FINISH is caught even
// though it would run a moment later.
func TestM1PrelaunchAbandonSettlesBeforeFinish(t *testing.T) {
	const m = domain.MissionID("m_uf")
	st, ln, cfg, bindings := tokenRig(t, m)
	ctx := context.Background()
	const a = domain.AttemptID("r_uf_1")
	resp, err := st.Execute(ctx, "fabd", "start:r_uf_1", domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := cfg.Credential.Policy.PrepareLaunch(bindings, cfg.Credential.IDKey, string(m), string(a), nil); err != nil {
		t.Fatal(err)
	}
	s := New(st, cfg, ln)
	finished, settledAtFinish := false, false
	s.afterFinishExec = func(_ store.Response, err error) error {
		if err == nil {
			finished = true
			_, serr := os.Lstat(filepath.Join(bindings, string(a)+".settled"))
			settledAtFinish = serr == nil
		}
		return err
	}
	s.owedUnstarted(a, true)
	if err := s.abandonPrelaunch(ctx, m, a, fence, "test: refused"); err != nil {
		t.Fatal(err)
	}
	if !finished {
		t.Fatal("fixture: the prelaunch FINISH did not commit")
	}
	if !settledAtFinish {
		t.Fatal("the binding was not settled when the prelaunch FINISH committed")
	}
}

// F2, second half on its own: the scan settles an unsettled binding whose
// attempt never launched (START, binding, no ACK, no prelaunch FINISH), as
// unstarted: no VerifyGone, no hold.
func TestM1ScanSettlesNeverLaunchedBinding(t *testing.T) {
	const m = domain.MissionID("m_us")
	st, ln, cfg, bindings := tokenRig(t, m)
	st.Lease = 300 * time.Millisecond
	ctx := context.Background()
	const a = domain.AttemptID("r_us_1")
	if _, err := st.Execute(ctx, "fabd", "start:r_us_1", domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Credential.Policy.PrepareLaunch(bindings, cfg.Credential.IDKey, string(m), string(a), nil); err != nil {
		t.Fatal(err)
	}
	waitLeaseLapse(t, st, string(a))
	verified := 0
	cfg.VerifyGone = func(run domain.AttemptID, _ time.Time) (host.StopReceipt, error) {
		verified++
		return host.StopReceipt{Run: run}, errors.New("unverified")
	}
	s := New(st, cfg, ln)
	s.prober = &fakeProber{alive: false, known: true}
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(bindings, string(a)+".settled")); err != nil {
		t.Fatalf("never-launched binding not settled (attempt %s)", attemptState(t, st, string(a)))
	}
	if held, _ := st.OperatorHeld(ctx, m); held || verified != 0 {
		t.Fatalf("held=%v VerifyGone calls=%d; a never-launched attempt needs neither", held, verified)
	}
}
