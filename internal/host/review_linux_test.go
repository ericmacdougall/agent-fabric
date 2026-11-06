//go:build linux

package host

// Failing-first tests ported from the 8f83662f review (RV-12: Stop fails
// open when the user bus is unreachable). They failed against 8f83662f.

import (
	"os"
	"testing"
	"time"
)

// RV-12 (H4): with the bus unreachable, inventory still works from the
// cgroup path captured at Start and Stop verifies by cgroup emptiness; with
// an unreadable cgroup path, Stop returns an error and Verified=false, never
// a false receipt.
func TestRV12_StopFailsClosed(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 (starts a sleeper in a unit)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	h, err := l.Start(g, []string{"sleep", "30"}, nil, devnull, devnull)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond)
	before, err := l.Inventory(h)
	if err != nil || len(before) == 0 {
		t.Fatalf("inventory before: %v %v", before, err)
	}
	oldX, oldB := os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	os.Setenv("XDG_RUNTIME_DIR", "/nonexistent-review")
	os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent-review/bus")
	defer func() {
		os.Setenv("XDG_RUNTIME_DIR", oldX)
		if oldB == "" {
			os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
		} else {
			os.Setenv("DBUS_SESSION_BUS_ADDRESS", oldB)
		}
	}()
	// Inventory does not need the bus.
	if inv, err := l.Inventory(h); err != nil || len(inv) == 0 {
		t.Fatalf("inventory without the bus: %v %v", inv, err)
	}
	// A handle whose cgroup path cannot be read is an error, not "no processes".
	bogus := h
	bogus.CGroup = "/nonexistent-review/app.slice/x.service"
	rc, err := l.Stop(bogus, "review", 300*time.Millisecond)
	if err == nil || rc.Verified {
		t.Fatalf("stop with an unreadable cgroup: want an error and Verified=false, got err=%v verified=%v", err, rc.Verified)
	}
	// The real stop verifies by cgroup emptiness without the bus.
	rc, err = l.Stop(h, "review", 2*time.Second)
	if err != nil || !rc.Verified || len(rc.Signalled) == 0 {
		t.Fatalf("stop without the bus: err=%v verified=%v signalled=%v", err, rc.Verified, rc.Signalled)
	}
	if after, _ := l.Inventory(h); len(after) != 0 {
		t.Fatalf("processes alive after a verified stop: %d", len(after))
	}
}
