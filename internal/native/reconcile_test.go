package native_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/sched"
	"fabric/internal/storetest"
)

// goodqa#15087 increment 4b: native liveness through the stored lock path,
// and the reconcile pass that quarantines a killed native run.

// lockChild starts a child process (this test binary) that creates path,
// takes its flock, optionally chmods it, and holds it until the returned
// stop is called. With exit set it takes the flock and exits at once,
// leaving the file behind (a dead holder).
func lockChild(t *testing.T, path string, mode os.FileMode, exit bool) (stop func()) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestNativeLockChildHelper$", "-test.count=1")
	c.Env = append(os.Environ(), "FABRIC_LOCK_CHILD_PATH="+path, "FABRIC_LOCK_CHILD_MODE="+mode.String())
	if exit {
		c.Env = append(c.Env, "FABRIC_LOCK_CHILD_EXIT=1")
	}
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "lock-child-ready" {
				ready <- true
				break
			}
		}
		for sc.Scan() { // drain so the child never blocks on its stdout
		}
		ready <- false
	}()
	stop = func() {
		in.Close()
		c.Process.Kill() //nolint:errcheck // usually already gone
		c.Wait()         //nolint:errcheck
	}
	select {
	case ok := <-ready:
		if !ok {
			stop()
			t.Fatal("the lock child exited before taking its lock")
		}
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("the lock child did not take its lock within 5 s")
	}
	if exit {
		c.Wait() //nolint:errcheck
	}
	t.Cleanup(stop)
	return stop
}

// TestNativeLockChildHelper is lockChild's child process.
func TestNativeLockChildHelper(t *testing.T) {
	path := os.Getenv("FABRIC_LOCK_CHILD_PATH")
	if path == "" {
		return // started only by lockChild; no SKIP line
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if m := os.Getenv("FABRIC_LOCK_CHILD_MODE"); m == "----------" {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
	os.Stdout.WriteString("lock-child-ready\n") //nolint:errcheck
	if os.Getenv("FABRIC_LOCK_CHILD_EXIT") == "1" {
		os.Exit(0) // the kernel drops the flock; the file stays
	}
	b := make([]byte, 1)
	os.Stdin.Read(b) //nolint:errcheck // blocks until the parent closes stdin
}

// NEG-13 (AC-7): only a flock held by a live process reads as alive.
func TestNativeLockProbe(t *testing.T) {
	const a domain.AttemptID = "r_probe1"
	locks := func(t *testing.T) string {
		d := filepath.Join(t.TempDir(), "native-locks")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(d, string(a)+".lock")
	}
	t.Run("NULL lock path", func(t *testing.T) {
		if native.ProbeLock(a, "") {
			t.Fatal("alive with no lock path")
		}
	})
	t.Run("no file", func(t *testing.T) {
		if native.ProbeLock(a, locks(t)) {
			t.Fatal("alive with no file")
		}
	})
	t.Run("FIFO", func(t *testing.T) {
		p := locks(t)
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan bool, 1)
		start := time.Now()
		go func() { done <- native.ProbeLock(a, p) }()
		select {
		case alive := <-done:
			if alive || time.Since(start) > time.Second {
				t.Fatalf("FIFO: alive %v after %s; want gone within 1 s", alive, time.Since(start))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the probe blocked on a FIFO for 5 s")
		}
	})
	t.Run("symlink to a live child's lock", func(t *testing.T) {
		p := locks(t)
		target := filepath.Join(t.TempDir(), "native-locks", string(a)+".lock")
		lockChild(t, target, 0o600, false)
		if !native.ProbeLock(a, target) {
			t.Fatal("setup: the child's own file does not read alive")
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if native.ProbeLock(a, p) {
			t.Fatal("a symlink to a held lock reads alive")
		}
	})
	t.Run("mode 000", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Fatal("this case needs a non-root user: root opens a mode 000 file")
		}
		p := locks(t)
		lockChild(t, p, 0, false)
		if native.ProbeLock(a, p) {
			t.Fatal("a lock file the prober cannot open reads alive")
		}
	})
	t.Run("dead child", func(t *testing.T) {
		p := locks(t)
		lockChild(t, p, 0o600, true)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("setup: the dead child's file is missing: %v", err)
		}
		if native.ProbeLock(a, p) {
			t.Fatal("a file whose holder died reads alive")
		}
	})
	t.Run("live child", func(t *testing.T) { // positive control
		p := locks(t)
		stop := lockChild(t, p, 0o600, false)
		if !native.ProbeLock(a, p) {
			t.Fatal("a flock held by a live child reads gone")
		}
		stop()
		if native.ProbeLock(a, p) {
			t.Fatal("still alive after the child stopped")
		}
	})
	t.Run("prober with another STATE_DIR and HOME reads the stored path", func(t *testing.T) {
		ctx := context.Background()
		st := storetest.Open(t)
		st.Lease = 2 * time.Second
		stateDir(t) // the runner's FABRIC_STATE_DIR (A)
		e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
		seedRunning(t, st, e, "", 0)
		lock, err := native.LockPath(domain.AttemptID(e.g.Attempt))
		if err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := st.Pool().QueryRow(ctx, "SELECT lock_path FROM attempts WHERE id = $1", e.g.Attempt).Scan(&stored); err != nil || stored != lock {
			t.Fatalf("stored lock path %q (%v), want %q", stored, err, lock)
		}
		stop := lockChild(t, lock, 0o600, false) // the live runner's flock
		if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1", e.g.Attempt); err != nil {
			t.Fatal(err)
		}
		// The prober runs with another state dir (B) and another HOME.
		t.Setenv("FABRIC_STATE_DIR", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		h := hostid.Default()
		if _, err := sched.ReconcileHost(ctx, st, h, h, nil, native.ProbeLock); err != nil {
			t.Fatal(err)
		}
		if s := attemptState(t, st, e.g.Attempt); s != string(domain.AttemptUnknown) {
			t.Fatalf("a live run under another STATE_DIR is %s, want UNKNOWN (alive, kept)", s)
		}
		stop()
		if _, err := sched.ReconcileHost(ctx, st, h, h, nil, native.ProbeLock); err != nil {
			t.Fatal(err)
		}
		if s := attemptState(t, st, e.g.Attempt); s != string(domain.AttemptQuarantined) {
			t.Fatalf("after the runner died the attempt is %s, want QUARANTINED", s)
		}
	})
}

// NEG-12 (AC-7): a native run killed with SIGKILL ends QUARANTINED within
// one lease plus one reconcile tick, settles nothing, pauses the mission,
// and frees its slot after unpause and release. Real time: lease expiry
// uses the database's now().
func TestNativeKilledQuarantined(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	st := storetest.Open(t)
	const lease, tick = 2 * time.Second, time.Second
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_1", "read", `{"path":"notes.txt"}`, 50, 5)},
		fakeapi.Step{Events: fakeapi.Text("never seen", 50, 5), Stall: 500 * time.Millisecond}, // per event; the kill comes first
	)
	grant := filepath.Join(t.TempDir(), "grant.json")
	b, _ := json.Marshal(e.g)
	if err := os.WriteFile(grant, b, 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestNativeChildHelper$", "-test.count=1")
	child.Env = append(os.Environ(), storetest.ChildDSNEnv+"="+storetest.DSN, "FABRIC_NATIVE_CHILD_GRANT="+grant,
		"FABRIC_NATIVE_CHILD_LEASE="+lease.String())
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }() //nolint:errcheck
	deadline := time.Now().Add(10 * time.Second)
	for len(e.srv.Requests()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the child never reached its second request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	child.Wait() //nolint:errcheck
	killed := time.Now()
	h := hostid.Default()
	var took time.Duration
	for {
		if _, err := sched.ReconcileHost(ctx, st, h, h, nil, native.ProbeLock); err != nil {
			t.Fatal(err)
		}
		if attemptState(t, st, e.g.Attempt) == string(domain.AttemptQuarantined) {
			took = time.Since(killed)
			break
		}
		if time.Since(killed) > 10*time.Second {
			t.Fatalf("attempt %s still %s 10 s after SIGKILL, want QUARANTINED", e.g.Attempt, attemptState(t, st, e.g.Attempt))
		}
		time.Sleep(tick)
	}
	t.Logf("QUARANTINED %s after SIGKILL (lease %s, tick %s)", took, lease, tick)
	if bound := lease + tick + 500*time.Millisecond; took > bound {
		t.Fatalf("QUARANTINED %s after SIGKILL, over one lease plus one tick (%s)", took, bound)
	}
	if n := count(t, st, "SELECT count(*) FROM commands WHERE kind = 'FINISH_ATTEMPT'"); n != 0 {
		t.Fatalf("%d finish commands for a killed run, want 0", n)
	}
	if got := finishOutcomes(t, st, e.g.Mission); len(got) != 0 {
		t.Fatalf("finish outcomes %v for a killed run, want none", got)
	}
	var execState string
	if err := st.Pool().QueryRow(ctx, "SELECT execution FROM missions WHERE id = $1", e.g.Mission).Scan(&execState); err != nil || execState != string(domain.ExecPaused) {
		t.Fatalf("mission execution %q (%v), want PAUSED", execState, err)
	}
	m := domain.MissionID(e.g.Mission)
	if _, err := st.Execute(ctx, "op:tester", "unpause-killed", domain.Command{Kind: domain.CmdUnpauseMission, Mission: m, Reason: "checked the killed run"}); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	if _, err := st.Execute(ctx, "chat", "release-killed", domain.Command{Kind: domain.CmdRelease, Mission: m}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if n := count(t, st, "SELECT count(*) FROM missions WHERE id = $1 AND execution IN ('ADMITTED', 'RUNNING', 'WAITING', 'PAUSED')", e.g.Mission); n != 0 {
		t.Fatal("the mission still holds its slot after unpause and release")
	}
}
