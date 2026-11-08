//go:build linux

package host

import (
	"os"
	"path/filepath"
	"testing"

	"fabric/internal/egress"
)

// testManagerProfile is the worker-manager counterpart of testProfile, which
// stays the legacy Linux profile (goodqa#15235 S5b).
func testManagerProfile(t *testing.T) *LinuxManager {
	t.Helper()
	self := os.Getenv("FABRIC_FAB_BIN")
	if self == "" {
		t.Fatal("FABRIC_FAB_BIN must point at a built fab binary (for __sandbox)")
	}
	return &LinuxManager{Self: self, ReadOnly: []string{filepath.Dir(self)}, EgressConfig: &egress.Config{}, User: os.Getenv("FABRIC_WORKER_USER"),
		// A private 0700 lock directory: the default under the home must have no
		// group- or other-writable ancestor, which a test host need not satisfy.
		LockPath: filepath.Join(lockTestDir(t), "worker-manager.lock")}
}

// TestLinuxManagerNegativeControls is the live boundary proof for the worker
// manager. FABRIC_HOST_CONTROLS=1 opts in (it starts units and signals only
// the processes they create). With a dedicated worker every control must pass.
func TestLinuxManagerNegativeControls(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run the worker-manager controls")
	}
	if os.Getenv("FABRIC_WORKER_USER") == "" {
		t.Fatal("worker-manager controls require FABRIC_WORKER_USER")
	}
	p := testManagerProfile(t)
	results, err := p.NegativeControls(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("only %d controls ran", len(results))
	}
	var failed []string
	for _, r := range results {
		t.Logf("%-45s %-5v %s", r.Name, r.Passed, r.Detail)
		if !r.Passed {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("worker-manager controls failed: %v", failed)
	}
}
