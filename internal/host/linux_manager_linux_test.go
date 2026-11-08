//go:build linux

package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxWorkerUnitHasManagerIsolationProperty(t *testing.T) {
	l := &LinuxManager{Self: "/srv/fabric/bin/fab"}
	args, err := l.startArgs(controlGrant(t.TempDir()), "fab-current.service", []string{"worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{managerInaccessiblePathsProperty(os.Getuid()), managerSocketFamilyProperty, workerPrivatePIDsProperty, workerProtectProcProperty, workerProcSubsetProperty} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-p" && args[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("worker systemd-run args lack %q: %q", want, args)
		}
	}
}

func TestLinuxManagerIsolationPreflightFailsClosed(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'Unknown assignment InaccessiblePaths=' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A supported systemd, so the refusal comes from the preflight unit.
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\necho systemd 257\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := &LinuxManager{Self: "/unused/fab"}
	err := l.preflightManagerIsolation(os.Getuid())
	if err == nil {
		t.Fatal("preflight accepted a manager that rejected InaccessiblePaths")
	}
	if !strings.Contains(err.Error(), "preflight failed") || !strings.Contains(err.Error(), "Unknown assignment") {
		t.Fatalf("preflight error does not preserve the refusal cause: %v", err)
	}
}
