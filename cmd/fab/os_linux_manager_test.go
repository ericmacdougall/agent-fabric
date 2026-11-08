//go:build linux

package main

import (
	"testing"

	"fabric/internal/host"
)

// goodqa#15235 S5b condition 1: with the flag off (unset or any value other
// than "1"), the legacy profile is selected and the manager constructor is
// never called.
func TestLinuxProfileFlagOffSelectsLegacyAndNeverBuildsManager(t *testing.T) {
	orig := newLinuxManagerProfile
	t.Cleanup(func() { newLinuxManagerProfile = orig })
	newLinuxManagerProfile = func(string, []string) host.Profile {
		t.Fatal("worker manager constructed with FABRIC_LINUX_WORKER_MANAGER off")
		return nil
	}
	for _, v := range []string{"", "0", "true", "yes", " 1", "1 "} {
		t.Setenv("FABRIC_LINUX_WORKER_MANAGER", v)
		p := newLinuxProfile("/srv/fabric/bin/fab")
		if _, ok := p.(*host.Linux); !ok {
			t.Fatalf("flag %q: profile is %T, want *host.Linux", v, p)
		}
		if _, ok := p.(*host.LinuxManager); ok {
			t.Fatalf("flag %q selected the worker manager", v)
		}
	}
}

func TestLinuxProfileFlagOnSelectsManagerWithMediator(t *testing.T) {
	t.Setenv("FABRIC_LINUX_WORKER_MANAGER", "1")
	t.Setenv("FABRIC_DSN", "postgres://db.example:5432/fabric")
	m, ok := newLinuxProfile("/srv/fabric/bin/fab").(*host.LinuxManager)
	if !ok {
		t.Fatal("FABRIC_LINUX_WORKER_MANAGER=1 did not select the worker manager")
	}
	if m.EgressConfig == nil {
		t.Fatal("worker manager has no per-run egress mediator")
	}
	if len(m.TCPPorts) != 0 {
		t.Fatalf("worker manager retains direct TCP allowances: %v", m.TCPPorts)
	}
}
