//go:build darwin

package main

import (
	"errors"
	"net/url"
	"os"
	"strconv"

	"fabric/internal/host"
)

func newLinuxProfile(string) host.Profile { return nil }

// newDarwinProfile grants no writable root in $HOME (review C2); each run
// gets private configuration and caches under its run dir from the launch
// line. Outbound TCP is limited to 443 and the database port the fab
// binary itself needs, as on Linux (goodqa#15161).
func newDarwinProfile(sandboxExec string) host.Profile {
	ports := []int{443}
	if u, err := url.Parse(os.Getenv("FABRIC_DSN")); err == nil && u.Port() != "" {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			ports = append(ports, p)
		}
	}
	return &host.Darwin{SandboxExec: sandboxExec, TCPPorts: ports}
}

func sandboxMain([]string) error {
	return errors.New("__sandbox is Linux only (Landlock); macOS uses sandbox-exec")
}
