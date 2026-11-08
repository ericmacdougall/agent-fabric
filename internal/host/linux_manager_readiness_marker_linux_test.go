//go:build linux

package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// goodqa#15235 S5b red-team L1: a worker-replaced marker must never hang,
// exhaust or mislead the controller.
func TestReadLinuxReadyMarkerRefusesHostileReplacements(t *testing.T) {
	dir := t.TempDir()
	uid := os.Getuid()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("ready\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if ok, err := readLinuxReadyMarker(good, uid); !ok || err != nil {
		t.Fatalf("valid marker refused: %v %v", ok, err)
	}
	if ok, err := readLinuxReadyMarker(filepath.Join(dir, "absent"), uid); ok || err != nil {
		t.Fatalf("absent marker: %v %v", ok, err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"fifo": fifo, "devzero": "/dev/zero", "good": good}
	for name, target := range cases {
		link := filepath.Join(dir, "link-"+name)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		cases[name] = link
	}
	cases["fifoDirect"] = fifo
	hard := filepath.Join(dir, "hard")
	if err := os.Link(good, hard); err != nil {
		t.Fatal(err)
	}
	cases["hardlink"] = hard
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte("ready\nready\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cases["oversize"] = big
	for name, path := range cases {
		done := make(chan error, 1)
		go func() {
			ok, err := readLinuxReadyMarker(path, uid)
			if ok {
				err = nil
				done <- errAccepted
				return
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || err == errAccepted {
				t.Errorf("%s: hostile marker was not refused (err=%v)", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: marker read blocked", name)
		}
	}
	if ok, err := readLinuxReadyMarker(good+"x", uid); ok || err != nil {
		t.Fatalf("absent marker: %v %v", ok, err)
	}
}

// The owner must be the worker uid.
func TestReadLinuxReadyMarkerRefusesWrongOwner(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good")
	if err := os.WriteFile(good, []byte("ready\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if ok, err := readLinuxReadyMarker(good, os.Getuid()+1); ok || err == nil {
		t.Fatalf("marker of another owner accepted: %v %v", ok, err)
	}
}

var errAccepted = &acceptedErr{}

type acceptedErr struct{}

func (*acceptedErr) Error() string { return "accepted" }

func TestWaitUnitReadyNeverRereadsTheMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("ready\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	swapped := false
	l := &LinuxManager{unitReadinessTimeout: 300 * time.Millisecond, managerCallTimeout: time.Second,
		commandFn: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if !swapped {
				swapped = true
				_ = os.Remove(marker)
				if err := os.Symlink(fifo, marker); err != nil {
					t.Error(err)
				}
			}
			return exec.CommandContext(ctx, "printf", "LoadState=loaded\nActiveState=activating\n")
		}}
	done := make(chan error, 1)
	go func() {
		done <- l.waitUnitReady(Handle{Unit: "u.service", CGroup: "/sys/fs/cgroup/x"}, marker, os.Getuid())
	}()
	select {
	case err := <-done:
		// A re-read would fail on the swapped link; only the latch reaches the timeout.
		if err == nil || !swapped || !strings.Contains(err.Error(), "did not become ready") {
			t.Fatalf("want a readiness timeout after the swap, got %v (swapped=%v)", err, swapped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitUnitReady re-read the swapped marker and blocked")
	}
}

// goodqa#15235 S5b red-team L5: a failed worker uid lookup must not run the
// command with a guessed uid 0.
func TestAsWorkerRefusesFailedUIDLookup(t *testing.T) {
	l := &LinuxManager{User: "fab-worker", managerCallTimeout: time.Second,
		commandFn: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "id" {
				return exec.CommandContext(ctx, "false")
			}
			return exec.CommandContext(ctx, "true")
		}}
	err := l.runAsWorker("/usr/bin/systemctl", "--user", "status")
	if err == nil || !strings.Contains(err.Error(), "worker uid lookup") {
		t.Fatalf("want a uid lookup refusal, got %v", err)
	}
}
