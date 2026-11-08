//go:build linux

package host

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// goodqa#15174 (Kimi K2): only the worker manager's own init scope and bus
// service are infrastructure. A unit a worker starts in its manager, under
// any name (fab-job-*, dbus-*, anything in session.slice), is an escapee.
// Host jobs are disabled (goodqa#15082), so no fab-job unit is legitimate.
func TestManagerInfraIsExact(t *testing.T) {
	for _, c := range []struct {
		rel   string
		infra bool
	}{
		{"init.scope", true},
		{"session.slice/dbus.service", true},
		{"session.slice/dbus-broker.service", true},
		{"app.slice/dbus.service", true},
		{"app.slice/fab-job-escape-1.service", false},
		{"app.slice/dbus-escape.service", false},
		{"app.slice/dbus.service.d", false},
		{"session.slice/escape.service", false},
		{"session.slice/dbus.service/child", false},
		{"app.slice/run-u12.service", false},
	} {
		if got := managerInfra(c.rel); got != c.infra {
			t.Errorf("managerInfra(%q) = %v, want %v", c.rel, got, c.infra)
		}
	}
}

// TestSiblingUnitIsAnEscapee is the live K2 control: a unit named like a
// host job, started in the worker user's own manager, must be reported by
// the escapee inventory. Opt-in (it starts a 20 s unit as the worker user):
// FABRIC_HOST_CONTROLS=1 and FABRIC_WORKER_USER.
func TestSiblingUnitIsAnEscapee(t *testing.T) {
	user := os.Getenv("FABRIC_WORKER_USER")
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" || user == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 and FABRIC_WORKER_USER to run the live sibling-unit control")
	}
	l := &Linux{User: user}
	uid, err := l.workerUID()
	if err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("fab-job-k2probe-%d", time.Now().UnixNano()%1_000_000)
	start := exec.Command("sudo", "-n", "-u", user, "env",
		"XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(uid), fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
		"systemd-run", "--user", "--collect", "--quiet", "--unit", unit, "/bin/sleep", "20")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start sibling unit: %v: %s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("sudo", "-n", "-u", user, "env", "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(uid),
			fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
			"systemctl", "--user", "stop", unit).Run() //nolint:errcheck
	})
	var found bool
	for i := 0; i < 20 && !found; i++ {
		esc, err := uidProcesses(uid, map[int]bool{}, "/sys/fs/cgroup/nonexistent-run.service")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range esc {
			cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", p.PID))
			if strings.Contains(string(cg), unit) {
				found = true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		t.Fatalf("sibling unit %s in the worker manager is not reported as an escapee", unit)
	}
}
