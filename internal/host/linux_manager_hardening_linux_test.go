//go:build linux

package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcReadUncertaintyFailsClosed(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		root := t.TempDir()
		pid := 321
		dir := filepath.Join(root, "321")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		stat := "321 (worker) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 999\n"
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "cmdline"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := procIdentityAt(root, pid); err == nil {
			t.Fatal("cmdline read failure was treated as an absent process")
		}
		if _, exists, err := procIdentityAt(root, 322); err != nil || exists {
			t.Fatalf("explicit ENOENT should mean absence: exists=%v err=%v", exists, err)
		}
	})

	t.Run("uid and token", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "654")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tworker\nUid:\tnot-a-number\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := procUIDAt(root, 654); err == nil {
			t.Fatal("malformed UID record was ignored")
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tworker\nUid:\t1000\t1000\t1000\t1000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "environ"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := tokenCarriersAt(root, "token", map[int]bool{}, 1000); err == nil {
			t.Fatal("token environment read failure was ignored")
		}
		if err := os.Mkdir(filepath.Join(dir, "cgroup"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := uidProcessesAt(root, 1000, map[int]bool{}, "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/fab-run.service"); err == nil {
			t.Fatal("worker cgroup read failure was ignored")
		}
		if err := assertNoAbandonedRunsAt(root, 1000); err == nil {
			t.Fatal("abandoned-run scan ignored cgroup read uncertainty")
		}
	})
}

func TestSameSerialForgedHandleCannotReleaseRun(t *testing.T) {
	path := filepath.Join(lockTestDir(t), "worker-manager.lock")
	h := Handle{Run: "run-current", Token: "token", PID: 42, Unit: "fab-run-current.service", UnitSerial: 2, CGroup: "/sys/fs/cgroup/current"}
	current := &LinuxManager{LockPath: path, activeSerial: h.UnitSerial, active: h}
	if !current.runGate.tryLock() {
		t.Fatal("current in-process gate")
	}
	if err := current.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatal(err)
	}
	defer current.releaseRun() //nolint:errcheck
	for _, forged := range []Handle{
		func() Handle { h := h; h.Unit = "fab-forged.service"; return h }(),
		func() Handle { h := h; h.CGroup = "/sys/fs/cgroup/forged"; return h }(),
		func() Handle { h := h; h.Token = "forged"; return h }(),
	} {
		if rc, err := current.Stop(forged, "forged same-serial handle", 0); err == nil || rc.Verified {
			t.Fatalf("forged handle accepted: handle=%+v receipt=%+v err=%v", forged, rc, err)
		}
		if current.activeSerial != h.UnitSerial || current.active.Unit != h.Unit {
			t.Fatal("forged Stop changed the active generation")
		}
	}
	contender := &LinuxManager{LockPath: path}
	if !contender.runGate.tryLock() {
		t.Fatal("contender gate")
	}
	defer contender.releaseRun() //nolint:errcheck
	if err := contender.lockWorkerManager(os.Getuid()); !errors.Is(err, ErrWorkerManagerBusy) {
		t.Fatalf("forged Stop released the current flock: %v", err)
	}
}

func TestWorkerManagerLockReplacementRefusesRelease(t *testing.T) {
	dir := lockTestDir(t)
	path := filepath.Join(dir, "worker-manager.lock")
	l := &LinuxManager{LockPath: path, activeSerial: 1}
	if !l.runGate.tryLock() {
		t.Fatal("run gate")
	}
	if err := l.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.releaseRun(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("lock replacement was not detected: %v", err)
	}
	if l.runGate.tryLock() {
		l.runGate.unlock()
		t.Fatal("replacement reopened the run gate")
	}
	_ = l.runLock.Close()
	l.runLock = nil
	l.runLockInfo = nil
	l.runGate.unlock()
}

func TestDefaultWorkerManagerLockUsesPrivateControllerDirectory(t *testing.T) {
	base := lockTestDir(t)
	orig := lockRuntimeBase
	lockRuntimeBase = func() string { return base }
	t.Cleanup(func() { lockRuntimeBase = orig })
	// goodqa#15235 S5b red-team L2: the environment must not move the lock.
	var path string
	for _, env := range [][2]string{{t.TempDir(), t.TempDir()}, {"", t.TempDir()}, {"/nonexistent", "/nonexistent"}} {
		t.Setenv("XDG_RUNTIME_DIR", env[0])
		t.Setenv("HOME", env[1])
		got, err := workerManagerLockPath("", 2345)
		if err != nil {
			t.Fatal(err)
		}
		if path != "" && got != path {
			t.Fatalf("lock path moved with the environment: %s then %s", path, got)
		}
		path = got
	}
	if filepath.Dir(path) != filepath.Join(base, "fab", "locks") {
		t.Fatalf("unexpected lock path %s", path)
	}
	st, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("lock directory mode = %o", st.Mode().Perm())
	}
}

func TestWorkerManagerLockRejectsWritableAncestor(t *testing.T) {
	base := filepath.Join(t.TempDir(), "writable")
	private := filepath.Join(base, "private")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := workerManagerLockPath(filepath.Join(private, "worker.lock"), 1000); err == nil {
		t.Fatal("lock path beneath a replaceable directory was accepted")
	}
}
