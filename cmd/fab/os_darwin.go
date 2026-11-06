//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"

	"fabric/internal/host"
)

func newLinuxProfile(string) host.Profile { return nil }

func newDarwinProfile(sandboxExec string) host.Profile {
	return &host.Darwin{SandboxExec: sandboxExec, ExtraRW: []string{filepath.Join(os.Getenv("HOME"), ".claude"), filepath.Join(os.Getenv("HOME"), "Library", "Caches")}}
}

func sandboxMain([]string) error {
	return errors.New("__sandbox is Linux only (Landlock); macOS uses sandbox-exec")
}
