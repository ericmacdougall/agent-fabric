//go:build darwin

package main

import (
	"errors"

	"fabric/internal/host"
)

func newLinuxProfile(string) host.Profile { return nil }

// newDarwinProfile grants no writable root in $HOME (review C2); each run
// gets private configuration and caches under its run dir from the launch
// line.
func newDarwinProfile(sandboxExec string) host.Profile {
	return &host.Darwin{SandboxExec: sandboxExec}
}

func sandboxMain([]string) error {
	return errors.New("__sandbox is Linux only (Landlock); macOS uses sandbox-exec")
}
