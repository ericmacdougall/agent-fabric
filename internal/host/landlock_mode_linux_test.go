//go:build linux

package host

import "testing"

// goodqa#15235 S5b: only a worker-manager launch (readiness flags present)
// demands ABI 4 and the socket filter; the legacy sandbox keeps ABI 1.
func TestSandboxContainmentModeIsManagerOnly(t *testing.T) {
	cases := []struct {
		sb      Sandbox
		manager bool
		abi     int
	}{
		{Sandbox{}, false, 1},
		{Sandbox{ReadOnly: []string{"/x"}, TCPPorts: []int{443}}, false, 1},
		{Sandbox{ReadyFile: "/r/jobs/.fab-ready-u", ReadyCGroup: "/u.slice/u.service"}, true, 4},
		{Sandbox{ReadyFile: "/r/jobs/.fab-ready-u"}, true, 4},
		{Sandbox{ReadyCGroup: "/u.slice/u.service"}, true, 4},
	}
	for i, c := range cases {
		if got := managerContainment(c.sb); got != c.manager {
			t.Errorf("case %d: managerContainment=%v want %v", i, got, c.manager)
		}
		if got := sandboxMinABI(c.sb); got != c.abi {
			t.Errorf("case %d: sandboxMinABI=%d want %d", i, got, c.abi)
		}
	}
}

func TestCompleteSandboxReadyNoopForLegacyAndRefusesHalfConfig(t *testing.T) {
	if err := CompleteSandboxReady(Sandbox{}); err != nil {
		t.Fatalf("legacy sandbox readiness: %v", err)
	}
	if err := CompleteSandboxReady(Sandbox{ReadyFile: t.TempDir() + "/m"}); err == nil {
		t.Fatal("readiness with no cgroup was accepted")
	}
	if err := CompleteSandboxReady(Sandbox{ReadyFile: t.TempDir() + "/m", ReadyCGroup: "/not/this/cgroup.service"}); err == nil {
		t.Fatal("readiness from the wrong cgroup was accepted")
	}
}
