//go:build linux

package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUnitManager answers the systemctl calls of a stop: the run's unit is
// gone (inactive, no control group), list-units prints the current set and
// stop removes a unit from it.
type fakeUnitManager struct {
	mu      sync.Mutex
	units   map[string]bool
	stopped []string
	listErr bool
	ran     []string
}

func (f *fakeUnitManager) commandFn(t *testing.T) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ran = append(f.ran, name+" "+strings.Join(args, " "))
		if name == "id" {
			return exec.CommandContext(ctx, "echo", "61234")
		}
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "list-units"):
			if f.listErr {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'Failed to connect to bus' >&2; exit 1")
			}
			var lines []string
			for u := range f.units {
				lines = append(lines, "● "+u+" loaded inactive dead x")
			}
			sort.Strings(lines)
			return exec.CommandContext(ctx, "printf", "%s\n", strings.Join(lines, "\n"))
		case strings.Contains(joined, " show "):
			return exec.CommandContext(ctx, "printf", "LoadState=not-found\nActiveState=inactive\nControlGroup=\n")
		case strings.Contains(joined, " stop "):
			u := args[len(args)-1]
			f.stopped = append(f.stopped, u)
			delete(f.units, u)
			return exec.CommandContext(ctx, "true")
		case strings.Contains(joined, "systemd-run"):
			t.Errorf("a unit was launched: %s", joined)
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}
}

func unitSet(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// The fake worker uid (61234) owns no process, so Inventory's scan of /proc
// for worker-uid escapees finds none and needs no foreign environ reads.

// stoppedHandle is a handle whose collected unit's cgroup directory is gone
// while the manager's directory exists (Inventory's "collected" shape).
func stoppedHandle(t *testing.T) Handle {
	mgr := filepath.Join(t.TempDir(), "user@61234.service")
	if err := os.MkdirAll(mgr, 0o755); err != nil {
		t.Fatal(err)
	}
	return Handle{Run: "r_units", Token: "units-token-" + time.Now().Format("150405.000000000"), Unit: "fab-r_units.service",
		CGroup: filepath.Join(mgr, "app.slice", "fab-r_units.service")}
}

// goodqa#15976 AC-4 / NEG-5 (review H1): a timer the worker planted through
// its manager holds no process until it fires, so process inventory says
// the run is clean. The stop must still be unverified, name the timer, stop
// it, and verify only on a retry once it is gone.
func TestStopVerificationRefusesAPlantedTimer(t *testing.T) {
	f := &fakeUnitManager{units: unitSet("dbus.socket", "default.target", "launchpadlib-cache-clean.timer")}
	l := &LinuxManager{User: "fab-worker", managerCallTimeout: time.Second}
	l.commandFn = f.commandFn(t)
	baseline, err := l.workerUnits()
	if err != nil {
		t.Fatal(err)
	}
	l.unitBaseline = baseline
	h := stoppedHandle(t)
	// During the run: the worker plants a timer and its service; the host
	// mounts something and app.slice appears; the run's own unit lingers.
	f.units["fab-planted.timer"] = true
	f.units["fab-planted.service"] = true
	f.units["run-docker-netns-x.mount"] = true
	f.units["app.slice"] = true
	f.units[h.Unit] = true
	_, err = l.verifyStopped(h)
	if err == nil || !strings.Contains(err.Error(), "fab-planted.timer") || !strings.Contains(err.Error(), "fab-planted.service") {
		t.Fatalf("a planted timer did not make the stop unverified: %v", err)
	}
	if strings.Contains(err.Error(), ".mount") || strings.Contains(err.Error(), "app.slice") || strings.Contains(err.Error(), h.Unit) {
		t.Errorf("passive or own units counted as escapes: %v", err)
	}
	if got := strings.Join(f.stopped, " "); !strings.Contains(got, "fab-planted.timer") || !strings.Contains(got, "fab-planted.service") {
		t.Errorf("escaped units not stopped as cleanup: %v", f.stopped)
	}
	if _, err := l.verifyStopped(h); err != nil {
		t.Fatalf("the retried stop did not verify once the escapes were gone: %v", err)
	}
}

// Every other escape type counts; a listing failure or a missing baseline
// is never a verified stop.
func TestStopVerificationFailsClosed(t *testing.T) {
	for _, planted := range []string{"x.path", "x.socket", "x.scope", "x.automount", "x.service"} {
		f := &fakeUnitManager{units: unitSet("dbus.socket")}
		l := &LinuxManager{User: "fab-worker", managerCallTimeout: time.Second, unitBaseline: unitSet("dbus.socket")}
		l.commandFn = f.commandFn(t)
		f.units[planted] = true
		if _, err := l.verifyStopped(stoppedHandle(t)); err == nil {
			t.Errorf("%s did not make the stop unverified", planted)
		}
	}
	f := &fakeUnitManager{units: unitSet("dbus.socket"), listErr: true}
	l := &LinuxManager{User: "fab-worker", managerCallTimeout: time.Second, unitBaseline: unitSet("dbus.socket")}
	l.commandFn = f.commandFn(t)
	if _, err := l.verifyStopped(stoppedHandle(t)); err == nil {
		t.Error("a failed unit listing verified the stop")
	}
	f = &fakeUnitManager{units: unitSet("dbus.socket")}
	l = &LinuxManager{User: "fab-worker", managerCallTimeout: time.Second}
	l.commandFn = f.commandFn(t)
	if _, err := l.verifyStopped(stoppedHandle(t)); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Errorf("a stop without a start-time baseline verified: %v", err)
	}
}

// StartIO takes the baseline before anything launches; when it cannot, the
// start is refused and no unit is launched.
func TestStartRefusesWithoutAUnitBaseline(t *testing.T) {
	f := &fakeUnitManager{units: unitSet(), listErr: true}
	lockDir := t.TempDir()
	if err := os.Chmod(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	l := &LinuxManager{Self: "/nonexistent/fab", User: "fab-worker", LockPath: filepath.Join(lockDir, "mgr.lock"), managerCallTimeout: time.Second}
	l.commandFn = f.commandFn(t)
	g := controlGrant(t.TempDir())
	_, err := l.StartIO(g, []string{"true"}, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unit baseline") {
		t.Fatalf("want a baseline refusal, got %v (ran %v)", err, f.ran)
	}
}

func TestParseUnitList(t *testing.T) {
	got, err := parseUnitList([]byte("● failed.service loaded failed failed X\ndbus.socket loaded active running D-Bus\n\n"))
	if err != nil || !got["failed.service"] || !got["dbus.socket"] || len(got) != 2 {
		t.Fatalf("parse = %v, %v", got, err)
	}
	if _, err := parseUnitList([]byte("Failed to connect to bus\n")); err == nil {
		t.Error("an error line parsed as a unit list")
	}
}
