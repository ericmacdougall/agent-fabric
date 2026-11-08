package tools_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/tools"
)

// backgroundChild runs a leader that backgrounds a sleeper and exits at once,
// and returns the sleeper's pid. The sleeper is signalled at test end in case
// the confiner left it behind.
func backgroundChild(t *testing.T, f *fixture) (int, error) {
	t.Helper()
	res, err := f.conf.Run(context.Background(), tools.ExecSpec{
		Argv: []string{"/bin/sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!"}, Timeout: 10 * time.Second,
	})
	pid, convErr := strconv.Atoi(strings.TrimSpace(res.Output))
	if convErr != nil || pid <= 1 {
		t.Fatalf("leader output %q (run err %v): no child pid", res.Output, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid, err
}

// alive polls briefly so a just-signalled child has time to be reaped.
func alive(pid int, settle time.Duration) bool {
	deadline := time.Now().Add(settle)
	for {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Flag off is main's behaviour: a backgrounded child outlives Run.
func TestExitCleanupOffLeavesBackgroundChild(t *testing.T) {
	f := newFixture(t)
	pid, err := backgroundChild(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if !alive(pid, 300*time.Millisecond) {
		t.Fatal("with ExitCleanup off the background child is gone; flag-off behaviour changed")
	}
}

// Flag on: Run returns only after the backgrounded child is stopped.
func TestExitCleanupOnStopsBackgroundChild(t *testing.T) {
	f := newFixture(t)
	f.conf.ExitCleanup = true
	pid, err := backgroundChild(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if alive(pid, 2*time.Second) {
		t.Fatalf("background child %d survived Run with ExitCleanup on", pid)
	}
}

// recordingStarter is a process-free host: Wait returns at once (or only
// after Stop when blockWait), and Stop returns scripted results.
type recordingStarter struct {
	mu        sync.Mutex
	stops     int
	stopErrs  []error // per call; beyond the list, stopErr
	stopErr   error
	receipt   host.StopReceipt
	waitErr   error
	blockWait bool
	released  chan struct{}
}

func newRecordingStarter() *recordingStarter {
	return &recordingStarter{receipt: host.StopReceipt{Verified: true}, released: make(chan struct{})}
}

func (f *recordingStarter) Start(g host.Grant, _ []string, _ []string, stdout, _ *os.File) (host.Handle, error) {
	_, _ = stdout.WriteString("leader output")
	return host.Handle{Run: g.Run, Token: g.Token, Unit: "fake-unit"}, nil
}

func (f *recordingStarter) Wait(host.Handle) (int, error) {
	if f.blockWait {
		<-f.released
	}
	return 0, f.waitErr
}

func (f *recordingStarter) Stop(hd host.Handle, reason string, _ time.Duration) (host.StopReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	err := f.stopErr
	if f.stops <= len(f.stopErrs) {
		err = f.stopErrs[f.stops-1]
	}
	if err != nil {
		return host.StopReceipt{}, err
	}
	rc := f.receipt
	rc.Run, rc.Reason = hd.Run, reason
	if rc.Verified && len(rc.Survivors) == 0 && len(rc.Escaped) == 0 && f.blockWait {
		select {
		case <-f.released:
		default:
			close(f.released)
		}
	}
	return rc, nil
}

func (f *recordingStarter) stopCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func recordingConfiner(t *testing.T, s tools.Starter, exitCleanup bool) *tools.HostConfiner {
	t.Helper()
	return &tools.HostConfiner{Host: s, ExitCleanup: exitCleanup, Base: host.Grant{
		Run: domain.AttemptID("r_tool_cleanup"), RunDir: t.TempDir(), Token: "cleanup-token",
	}}
}

func TestExitCleanupOffNeverStopsAfterNormalExit(t *testing.T) {
	s := newRecordingStarter()
	s.waitErr = errors.New("wait failed") // main ignores it; flag off must too
	res, err := recordingConfiner(t, s, false).Run(context.Background(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: time.Second})
	if err != nil || res.Output != "leader output" {
		t.Fatalf("flag off: res %+v err %v, want main behaviour", res, err)
	}
	if got := s.stopCalls(); got != 0 {
		t.Fatalf("flag off: Stop called %d times after a normal exit", got)
	}
}

func TestExitCleanupOnStopsEverySequentialRun(t *testing.T) {
	s := newRecordingStarter()
	c := recordingConfiner(t, s, true)
	for call := 1; call <= 2; call++ {
		res, err := c.Run(context.Background(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: time.Second})
		if err != nil || res.Output != "leader output" {
			t.Fatalf("call %d: res %+v err %v", call, res, err)
		}
	}
	if got := s.stopCalls(); got != 2 {
		t.Fatalf("Stop calls %d, want one per started handle", got)
	}
}

func TestExitCleanupOnWaitErrorStillCleansAndPropagates(t *testing.T) {
	waitErr := errors.New("leader wait failed")
	s := newRecordingStarter()
	s.waitErr = waitErr
	s.stopErrs = []error{errors.New("transient Stop failure")}
	res, err := recordingConfiner(t, s, true).Run(context.Background(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: time.Second})
	if !errors.Is(err, waitErr) {
		t.Fatalf("Run error %v, want the Wait error", err)
	}
	if res.Output != "leader output" || s.stopCalls() != 2 {
		t.Fatalf("res %+v Stop calls %d, want output and a retried cleanup", res, s.stopCalls())
	}
}

func TestExitCleanupOnTimeoutAndCancellationCleanDetached(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		ctx     func() context.Context
	}{
		{"deadline", 20 * time.Millisecond, context.Background},
		{"cancellation", time.Hour, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRecordingStarter()
			s.blockWait = true // only a verified Stop releases Wait
			started := time.Now()
			res, err := recordingConfiner(t, s, true).Run(tc.ctx(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: tc.timeout})
			if err != nil || !res.TimedOut || res.Output != "leader output" {
				t.Fatalf("res %+v err %v, want timeout with output", res, err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("cleanup extended the deadline path")
			}
			if got := s.stopCalls(); got != 1 {
				t.Fatalf("Stop calls %d, want 1 despite a cancelled context", got)
			}
		})
	}
}

// A timed-out leader whose cleanup cannot be verified may still be running:
// Run must return the cleanup error instead of waiting for it forever.
func TestExitCleanupOnFailedTimeoutCleanupDoesNotHang(t *testing.T) {
	s := newRecordingStarter()
	s.blockWait = true // Wait never returns: no Stop is ever verified
	s.receipt.Verified = false
	c := recordingConfiner(t, s, true)
	type result struct {
		res tools.ExecResult
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := c.Run(context.Background(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: 20 * time.Millisecond})
		got <- result{res, err}
	}()
	select {
	case r := <-got:
		if r.err == nil || !strings.Contains(r.err.Error(), "not verified") || !r.res.TimedOut {
			t.Fatalf("res %+v err %v, want timeout plus unverified cleanup error", r.res, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked on a leader whose cleanup failed")
	}
}

func TestExitCleanupOnFailuresAreConservative(t *testing.T) {
	stopErr := errors.New("inventory failed")
	for _, tc := range []struct {
		name    string
		prepare func(*recordingStarter)
		want    string
	}{
		{"stop error", func(s *recordingStarter) { s.stopErr = stopErr }, "inventory failed"},
		{"unverified", func(s *recordingStarter) { s.receipt.Verified = false }, "not verified"},
		{"survivor", func(s *recordingStarter) { s.receipt.Survivors = []host.Process{{PID: 123}} }, "survivors"},
		{"escaped", func(s *recordingStarter) { s.receipt.Escaped = []host.Process{{PID: 456}} }, "escaped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRecordingStarter()
			tc.prepare(s)
			res, err := recordingConfiner(t, s, true).Run(context.Background(), tools.ExecSpec{Argv: []string{"leader"}, Timeout: time.Second})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run error %v, want %q", err, tc.want)
			}
			if res.Output != "leader output" {
				t.Fatalf("output %q lost beside the cleanup failure", res.Output)
			}
			if got := s.stopCalls(); got != 3 {
				t.Fatalf("Stop calls %d, want bounded three attempts", got)
			}
		})
	}
}
