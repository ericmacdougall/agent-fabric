package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15976 PR #57: the Darwin profile's startup control runs the
// provider with --version. On Darwin a fake-only start points it at the
// sealed copy launches exec, never the original path; an unpinned file is
// refused; on other hosts the value is unused and stays the resolved path.
func TestStartupControlProviderIsTheSealedCopy(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fakeclaude-live")
	if err := os.WriteFile(p, src, 0o755); err != nil {
		t.Fatal(err)
	}
	dp, err := newDaemonProvider(p, filepath.Join(t.TempDir(), "checkpoint.schema.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	dp.Launch.Resolve = pinnedFakeResolve(dp.Launch.Resolve, dp.SealRoot)
	got, err := startupControlProvider(&dp, false, "darwin")
	if err != nil {
		t.Fatalf("the pinned fake was refused: %v", err)
	}
	root, err := filepath.EvalSymlinks(dp.SealRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got == p || !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("startup control runs %s; want the sealed copy under %s", got, root)
	}
	// Replacing the original changes nothing the control runs.
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho REPLACEMENT\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(got); err != nil || !bytes.Equal(b, src) {
		t.Fatalf("the sealed copy changed with the original (%v)", err)
	}
	// The replaced (unpinned) file is refused at the next start.
	if _, err := startupControlProvider(&dp, false, "darwin"); err == nil || !strings.Contains(err.Error(), "goodqa#15976") {
		t.Fatalf("an unpinned provider was accepted for the startup control: %v", err)
	}
	// Elsewhere (unused): the resolved path, unchanged behaviour.
	if got, err := startupControlProvider(&dp, false, "linux"); err != nil || got != dp.Path {
		t.Fatalf("linux: %q %v; want %q", got, err, dp.Path)
	}
}
