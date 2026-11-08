//go:build linux

package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleHandleCannotReleaseNewRun(t *testing.T) {
	path := filepath.Join(lockTestDir(t), "worker-manager.lock")
	current := &LinuxManager{LockPath: path, activeSerial: 2}
	if !current.runGate.tryLock() {
		t.Fatal("current in-process gate")
	}
	if err := current.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatal(err)
	}
	stale := Handle{Run: "run-old", Unit: "fab-run-old.service", UnitSerial: 1}
	if _, err := current.Wait(stale); err == nil {
		t.Fatal("stale Wait was accepted")
	}
	rc, err := current.Stop(stale, "stale cleanup", 0)
	if err == nil {
		t.Fatal("stale Stop was accepted")
	}
	if rc.Verified {
		t.Fatal("stale Stop produced a verified receipt")
	}
	contender := &LinuxManager{LockPath: path}
	if !contender.runGate.tryLock() {
		t.Fatal("contender in-process gate")
	}
	if err := contender.lockWorkerManager(os.Getuid()); err == nil {
		t.Fatal("stale Stop released the current run's flock")
	}
	if err := current.releaseRun(); err != nil {
		t.Fatal(err)
	}
	if err := contender.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatalf("current cleanup did not release the flock: %v", err)
	}
	if err := contender.releaseRun(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerManagerFlockSerializesProfilesUntilRelease(t *testing.T) {
	path := filepath.Join(lockTestDir(t), "worker-manager.lock")
	first := &LinuxManager{LockPath: path}
	second := &LinuxManager{LockPath: path}
	if !first.runGate.tryLock() {
		t.Fatal("first in-process gate")
	}
	if err := first.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if !second.runGate.tryLock() {
		t.Fatal("second in-process gate")
	}
	if err := second.lockWorkerManager(os.Getuid()); err == nil {
		t.Fatal("second profile acquired a busy worker-manager flock")
	}
	if err := second.releaseRun(); err != nil {
		t.Fatal(err)
	}
	if second.runGate.tryLock() {
		second.runGate.unlock()
	} else {
		t.Fatal("failed flock acquisition left the second gate locked")
	}
	if err := first.releaseRun(); err != nil {
		t.Fatal(err)
	}
	if !second.runGate.tryLock() {
		t.Fatal("second gate did not reopen after first verified cleanup")
	}
	if err := second.lockWorkerManager(os.Getuid()); err != nil {
		t.Fatalf("flock was not released by verified cleanup: %v", err)
	}
	if err := second.releaseRun(); err != nil {
		t.Fatal(err)
	}
}

// fakeSystemdRun proves contention returns before exec: any attempted launch
// writes a marker through a fake systemd-run found first on PATH.
func fakeSystemdRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "systemd-run-launched")
	script := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf launched > \"$FAB_TEST_SYSTEMD_RUN_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAB_TEST_SYSTEMD_RUN_MARKER", marker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func startWithDevNull(t *testing.T, l *LinuxManager) error {
	t.Helper()
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, err = l.Start(mgrControlGrant(t.TempDir()), []string{"worker"}, nil, out, out)
	return err
}

func assertNoSystemdRun(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("systemd-run launched during prelaunch contention: %v", err)
	}
}

func TestWorkerManagerBusyIsDefiniteAndPrelaunch(t *testing.T) {
	t.Run("in-process run gate", func(t *testing.T) {
		marker := fakeSystemdRun(t)
		l := &LinuxManager{Self: "/unused/fab"}
		if !l.runGate.tryLock() {
			t.Fatal("acquire test run gate")
		}
		defer l.runGate.unlock()
		if err := startWithDevNull(t, l); !errors.Is(err, ErrWorkerManagerBusy) {
			t.Fatalf("run-gate contention error %v does not match ErrWorkerManagerBusy", err)
		}
		assertNoSystemdRun(t, marker)
	})

	t.Run("cross-process flock", func(t *testing.T) {
		marker := fakeSystemdRun(t)
		path := filepath.Join(lockTestDir(t), "worker-manager.lock")
		holder := &LinuxManager{LockPath: path}
		if !holder.runGate.tryLock() {
			t.Fatal("acquire holder run gate")
		}
		if err := holder.lockWorkerManager(os.Getuid()); err != nil {
			t.Fatal(err)
		}
		defer holder.releaseRun() //nolint:errcheck

		contender := &LinuxManager{Self: "/unused/fab", LockPath: path}
		defer contender.releaseRun() //nolint:errcheck
		if err := startWithDevNull(t, contender); !errors.Is(err, ErrWorkerManagerBusy) {
			t.Fatalf("flock contention error %v does not match ErrWorkerManagerBusy", err)
		}
		assertNoSystemdRun(t, marker)
	})
}

func TestWorkerManagerLockOpenErrorIsNotBusy(t *testing.T) {
	l := &LinuxManager{LockPath: t.TempDir()}
	err := l.lockWorkerManager(os.Getuid())
	if err == nil {
		t.Fatal("opening a directory as the lock file succeeded")
	}
	if errors.Is(err, ErrWorkerManagerBusy) {
		t.Fatalf("non-contention lock error was classified as busy: %v", err)
	}
}

// lockTestDir is a controller-owned 0700 directory, as the worker-manager
// lock requires (t.TempDir() subdirectories are 0755 on Linux).
func lockTestDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}
