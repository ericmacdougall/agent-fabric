package host

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWorkerManagerEscapeHasNoSiblingNameExemptions(t *testing.T) {
	cases := []struct {
		rel    string
		escape bool
	}{
		{"app.slice/fab-current.service", false},
		{"init.scope", false},
		{"init.scope/child", true},
		{"app.slice/fab-sibling.service", true},
		{"app.slice/fab-reused.service", true},
		{"app.slice/fab-job-attacker.service", true},
		{"app.slice/dbus.service", true},
		{"app.slice/dbus-evil.service", true},
		{"session.slice/session-1.scope", true},
	}
	for _, tc := range cases {
		if got := workerManagerEscape(tc.rel, "app.slice/fab-current.service"); got != tc.escape {
			t.Errorf("workerManagerEscape(%q)=%v, want %v", tc.rel, got, tc.escape)
		}
	}
}

func TestRunGateRejectsConcurrentStartUntilVerifiedStop(t *testing.T) {
	var gate runGate
	if !gate.tryLock() {
		t.Fatal("initial Start did not acquire the run gate")
	}
	const contenders = 32
	var wg sync.WaitGroup
	var acquired atomic.Int32
	begin := make(chan struct{})
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			defer wg.Done()
			<-begin
			if gate.tryLock() {
				acquired.Add(1)
				gate.unlock()
			}
		}()
	}
	close(begin)
	wg.Wait()
	if acquired.Load() != 0 {
		t.Fatalf("%d concurrent Starts entered an active run", acquired.Load())
	}
	// Simulate Wait returning: no gate operation is permitted at leader exit.
	if gate.tryLock() {
		gate.unlock()
		t.Fatal("leader exit released serialization before verified Stop")
	}
	// A survivor-free Stop unlocks; only then may a subsequent run start.
	gate.unlock()
	if !gate.tryLock() {
		t.Fatal("verified Stop did not release the next Start")
	}
	gate.unlock()
}

func TestRunGateAndPolicyFailClosedOnAbandonedCleanupAndReuse(t *testing.T) {
	var gate runGate
	if !gate.tryLock() {
		t.Fatal("initial lock failed")
	}
	// Abandoned/incomplete cleanup has no verified Stop and therefore no
	// unlock. A second profile in this process must remain refused.
	if gate.tryLock() {
		gate.unlock()
		t.Fatal("abandoned cleanup permitted another Start")
	}
	for _, rel := range []string{"app.slice/fab-reused.service", "app.slice/fab-current.service.evil"} {
		if !workerManagerEscape(rel, "app.slice/fab-current.service") {
			t.Errorf("reused sibling %q was exempted by a name snapshot", rel)
		}
	}
	gate.unlock()
}

func TestWorkerManagerSocketEnvironmentCannotBeOverridden(t *testing.T) {
	base := []string{"FAB_RUN=token", "HOME=/home/worker", "PATH=/usr/bin", "USER=worker", "TMPDIR=/tmp"}
	for _, entry := range []string{
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
		"XDG_RUNTIME_DIR=/run/user/1000",
	} {
		if _, _, err := WorkerEnv(base); err != nil {
			t.Fatalf("base environment refused, so the test would be vacuous: %v", err)
		}
		// The Linux manager builds the worker environment with WorkerEnv,
		// whose allow-list refuses these names (goodqa#15235 S5b; the Kimi
		// test used the Windows-only mergeEnvironment).
		if _, _, err := WorkerEnv(append(append([]string{}, base...), entry)); err == nil {
			t.Errorf("worker-manager socket override %q was accepted", entry)
		}
	}
}

func TestManagerInaccessiblePathsCoversEveryControlSocket(t *testing.T) {
	property := managerInaccessiblePathsProperty(1042)
	for _, path := range []string{
		"-/run/user/1042/bus",
		"-/run/user/1042/systemd/private",
		"-/run/user/1042/systemd/notify",
		"-/run/dbus/system_bus_socket",
		"-/run/systemd/private",
		"-/run/systemd/notify",
	} {
		if !strings.Contains(property, path) {
			t.Errorf("manager isolation property %q omits %s", property, path)
		}
	}
	for _, path := range workerManagerControlSockets(1042) {
		if strings.HasPrefix(path, "-") || !strings.HasPrefix(path, "/") {
			t.Errorf("preflight socket path %q is not an absolute, unprefixed path", path)
		}
		if !strings.Contains(property, "-"+path) {
			t.Errorf("manager isolation property omits preflight socket %s", path)
		}
	}
}
