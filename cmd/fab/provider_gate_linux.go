//go:build linux

package main

import (
	"fmt"
	"strings"

	"fabric/internal/host"
)

// managerSupport reports whether the worker manager can enforce its
// boundary on this host; a variable for tests.
var managerSupport = func(m *host.LinuxManager) error { return m.CheckSupport() }

// realProviderGate (goodqa#15976 AC-2): on Linux a real provider runs only
// under host.LinuxManager, with a dedicated worker account, on a host whose
// systemd supports the manager. The legacy host.Linux profile lets a worker
// reach its own systemd user manager and start a sibling unit outside its
// cgroup, Landlock domain and network rules (review C1), so it never runs
// one. The check reads configuration and the systemd version only.
func realProviderGate(p host.Profile) error {
	var reasons []string
	user := ""
	switch m := p.(type) {
	case *host.LinuxManager:
		user = m.User
		if user != "" {
			if err := managerSupport(m); err != nil {
				reasons = append(reasons, "the worker manager is unsupported on this host: "+err.Error())
			}
		}
	case *host.Linux:
		user = m.User
		reasons = append(reasons, "the legacy Linux profile does not contain a worker (it reaches its own systemd manager, review C1); set FABRIC_LINUX_WORKER_MANAGER=1 on a host whose systemd supports it")
	default:
		reasons = append(reasons, fmt.Sprintf("profile %T is not the Linux worker manager", p))
	}
	if user == "" {
		reasons = append(reasons, "FABRIC_WORKER_USER is empty (a real provider needs a dedicated worker account)")
	}
	if len(reasons) > 0 {
		return realProviderRefused{strings.Join(reasons, "; ")}
	}
	return nil
}
