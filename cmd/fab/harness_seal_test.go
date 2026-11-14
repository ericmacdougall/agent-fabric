package main

import (
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15976 PR #57 review F1: `fab harness run --adapter claude-code`
// must execute the bytes its gate verified. After the gate passes (the
// harnessAfterGate pause), the file at the looked-up path is replaced, or
// the symlink named by --claude-bin is repointed, to a binary that leaves a
// marker. The replacement must never run, and the verified fake's healthy
// session must still pass.
func TestHarnessRunExecutesTheVerifiedBytes(t *testing.T) {
	for _, viaSymlink := range []bool{false, true} {
		name := "file replaced"
		if viaSymlink {
			name = "symlink repointed"
		}
		t.Run(name, func(t *testing.T) {
			ok := writeFakeClaude(t, okClaude)
			bin := ok
			if viaSymlink {
				bin = filepath.Join(t.TempDir(), "claude")
				if err := os.Symlink(ok, bin); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(t.TempDir(), "replacement-ran")
			repl := filepath.Join(t.TempDir(), "replacement")
			if err := os.WriteFile(repl, []byte("#!/bin/sh\ntouch '"+marker+"'\n"+okClaude), 0o755); err != nil {
				t.Fatal(err)
			}
			harnessAfterGate = func() {
				if viaSymlink {
					tmp := bin + ".new"
					if err := os.Symlink(repl, tmp); err != nil {
						t.Error(err)
					}
					if err := os.Rename(tmp, bin); err != nil {
						t.Error(err)
					}
					return
				}
				if err := os.Rename(repl, ok); err != nil {
					t.Error(err)
				}
			}
			defer func() { harnessAfterGate = func() {} }()
			err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", bin, "--prompt", "hi", "--wait", "1s")
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("the replacement ran after the gate verified %s (run err: %v)", bin, err)
			}
			if err != nil {
				t.Fatalf("the verified fake's session failed: %v", err)
			}
		})
	}
}
