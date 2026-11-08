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

func writeSuccessfulStream(t *testing.T, cfg Config, attempt domain.AttemptID) {
	t.Helper()
	dir := filepath.Join(cfg.WorkRoot, "run", string(attempt))
	if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(initEv+"\n"+okResult+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitSettlement(t *testing.T, settled <-chan struct{}) {
	t.Helper()
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("worker settlement did not finish")
	}
}

// runAndFinish launches one worker, lets it exit with a successful stream,
// and waits for the settlement attempt to end.
func runAndFinish(t *testing.T, s *Scheduler, ln *fakeLauncher, cfg Config, mission domain.MissionID, attempt domain.AttemptID) {
	t.Helper()
	settled := make(chan struct{})
	s.afterWaitDone = func() { close(settled) }
	if err := s.runOnce(context.Background(), mission); err != nil {
		t.Fatal(err)
	}
	writeSuccessfulStream(t, cfg, attempt)
	close(ln.block)
	waitSettlement(t, settled)
}

func settlementCommands(t *testing.T, st *store.Store, attempt domain.AttemptID) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM commands WHERE command_id = ANY($1)`,
		[]string{"finish:" + string(attempt), "verify:" + string(attempt)}).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Flag off is main's behaviour: a finished worker settles with no Stop.
func TestExitCleanupOffSettlesWithoutStop(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_cleanup_off")
	s := New(st, cfg, ln)
	runAndFinish(t, s, ln, cfg, "m_cleanup_off", "r_cleanup_off_1")
	if n := atomic.LoadInt32(&ln.stopCalls); n != 0 {
		t.Fatalf("Stop calls %d with ExitCleanup off, want 0", n)
	}
	if got := attemptState(t, st, "r_cleanup_off_1"); got != string(domain.AttemptVerified) {
		t.Fatalf("attempt state %s, want VERIFIED", got)
	}
}

func TestExitCleanupOnStopsAndVerifiesBeforeSuccess(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_cleanup_ok")
	cfg.ExitCleanup = true
	ln.stopReceipt.Verified = true
	s := New(st, cfg, ln)
	runAndFinish(t, s, ln, cfg, "m_cleanup_ok", "r_cleanup_ok_1")
	if n := atomic.LoadInt32(&ln.stopCalls); n != 1 {
		t.Fatalf("Stop calls %d, want 1 after a normal exit", n)
	}
	if got := attemptState(t, st, "r_cleanup_ok_1"); got != string(domain.AttemptVerified) {
		t.Fatalf("attempt state %s, want VERIFIED after verified cleanup", got)
	}
}

// An unverified cleanup never settles success: the attempt stays RUNNING
// with no finish or verify command, and its lease is left to expire.
func TestExitCleanupOnFailureNeverSettlesSuccess(t *testing.T) {
	for i, tc := range []struct {
		name    string
		receipt host.StopReceipt
		stopErr error
	}{
		{"stop error", host.StopReceipt{Verified: true}, errors.New("inventory failed")},
		{"unverified receipt", host.StopReceipt{Verified: false}, nil},
		{"survivor", host.StopReceipt{Verified: true, Survivors: []host.Process{{PID: 123}}}, nil},
		{"escaped process", host.StopReceipt{Verified: true, Escaped: []host.Process{{PID: 456}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := string(rune('a' + i))
			mission := domain.MissionID("m_cleanup_bad_" + suffix)
			attempt := domain.AttemptID("r_cleanup_bad_" + suffix + "_1")
			st, ln, cfg := ownerSetup(t, mission)
			cfg.ExitCleanup = true
			ln.stopReceipt, ln.stopErr = tc.receipt, tc.stopErr
			s := New(st, cfg, ln)
			var sleeps []time.Duration
			s.stopSleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
			runAndFinish(t, s, ln, cfg, mission, attempt)
			if n := atomic.LoadInt32(&ln.stopCalls); n != 3 {
				t.Fatalf("Stop calls %d, want a bounded three", n)
			}
			if len(sleeps) != 2 || sleeps[0] != 10*time.Millisecond || sleeps[1] != 20*time.Millisecond {
				t.Fatalf("retry pauses %v, want [10ms 20ms]", sleeps)
			}
			if got := attemptState(t, st, string(attempt)); got != string(domain.AttemptRunning) {
				t.Fatalf("attempt state %s after failed cleanup, want RUNNING until lease expiry", got)
			}
			if n := settlementCommands(t, st, attempt); n != 0 {
				t.Fatalf("%d settlement commands after failed cleanup, want 0", n)
			}
		})
	}
}

// A Wait error does not end ownership: cleanup still runs (and retries), and
// the attempt is still left unsettled because its exit is unknown.
func TestExitCleanupOnWaitErrorStillCleansWithoutSettlement(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_wait_err")
	cfg.ExitCleanup = true
	ln.waitErr = errors.New("injected Wait failure")
	ln.stopReceipt.Verified = true
	ln.stopErrs = []error{errors.New("transient Stop failure")}
	s := New(st, cfg, ln)
	s.stopSleep = func(context.Context, time.Duration) error { return nil }
	runAndFinish(t, s, ln, cfg, "m_wait_err", "r_wait_err_1")
	if n := atomic.LoadInt32(&ln.stopCalls); n != 2 {
		t.Fatalf("Stop calls %d, want a failure plus a verified retry", n)
	}
	if n := settlementCommands(t, st, "r_wait_err_1"); n != 0 {
		t.Fatalf("%d settlement commands after a Wait error, want 0", n)
	}
}

// noStopLauncher cannot stop a worker, so ExitCleanup cannot be honoured.
type noStopLauncher struct{}

func (noStopLauncher) Start(host.Grant, []string, []string, *os.File, *os.File) (host.Handle, error) {
	return host.Handle{}, errors.New("must not start")
}
func (noStopLauncher) Wait(host.Handle) (int, error) { return 0, nil }

func TestExitCleanupNeedsAStoppingLauncher(t *testing.T) {
	st, _, cfg := ownerSetup(t, "m_nostop")
	cfg.ExitCleanup = true
	s := New(st, cfg, noStopLauncher{})
	if _, err := s.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "Stop") {
		t.Fatalf("Tick with a launcher that cannot Stop: %v, want a configuration refusal", err)
	}
}
