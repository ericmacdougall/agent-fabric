//go:build darwin

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15976 AC-2 / NEG-4 (Darwin half): M1 refuses a real provider on
// macOS outright; the pinned fakes still start, fake-only.
func TestDarwinRefusesRealProviders(t *testing.T) {
	err := realProviderGate(nil)
	if err == nil || !strings.Contains(err.Error(), "goodqa#15976") || exitCode(err) != 3 {
		t.Fatalf("want a goodqa#15976 exit-3 refusal, got %v", err)
	}
	pinned, _ := filepath.Abs(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"))
	if fakeOnly, err := daemonProviderGate(false, false, pinned, nil); err != nil || !fakeOnly {
		t.Fatalf("the pinned fake: fakeOnly=%v err=%v", fakeOnly, err)
	}
	if _, err := daemonProviderGate(false, false, "/bin/sh", nil); err == nil {
		t.Fatal("a real provider started on macOS")
	}
	if err := nativeRunnerGate(); err == nil {
		t.Fatal("the native runner is on")
	}
}
