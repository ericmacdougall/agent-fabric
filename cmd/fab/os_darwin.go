//go:build darwin

package main

import (
	"errors"

	"fabric/internal/host"
)

func newLinuxProfile(string) host.Profile { return nil }

// darwinLaunchRO is set by `fab daemon` before it builds the profile: the
// reviewed read-only launch inputs (goodqa#15161 review F4).
var darwinLaunchRO []string

// newDarwinProfile grants no writable root in $HOME (review C2); each run
// gets private configuration and caches under its run dir from the launch
// line. Outbound TCP is limited to 443 and the database ports, as on Linux
// (goodqa#15161).
func newDarwinProfile(sandboxExec string) host.Profile {
	return &host.Darwin{SandboxExec: sandboxExec, TCPPorts: workerTCPPorts(), ExtraRO: darwinLaunchRO}
}

func sandboxMain([]string) error {
	return errors.New("__sandbox is Linux only (Landlock); macOS uses sandbox-exec")
}
