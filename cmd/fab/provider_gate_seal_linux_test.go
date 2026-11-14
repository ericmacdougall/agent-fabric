//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// goodqa#15976 review F1: verification and execution must use the same
// bytes. Each case verifies a trusted pinned fake through fabd's own launch
// (claude.Argv), then, in the window before exec, replaces the original
// file (or the stdin fake's delegate) with a binary that leaves a marker,
// then resumes the launch by executing what the launch named. The marker
// must never appear and the fake's own output must. Only symbols that
// predate the fix are used, so this file runs unchanged against 72bb1e2.
func TestSealedFakeIgnoresAReplacementAfterVerification(t *testing.T) {
	fake := filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh")
	stdinFake := filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh")
	for _, c := range []struct {
		name     string
		provider string // the file fabd is started with
		replace  string // the file swapped after verification
		run      func(argv0 string) string
	}{
		{"recorded fake replaced", "fakeclaude-live", "fakeclaude-live", func(a string) string { return a }},
		{"stdin fake's delegate replaced", "fakeclaude-stdin", "fakeclaude-live", func(a string) string { return filepath.Join(filepath.Dir(a), "fakeclaude-live") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			copyFile(t, fake, filepath.Join(bin, "fakeclaude-live"))
			if c.provider == "fakeclaude-stdin" {
				copyFile(t, stdinFake, filepath.Join(bin, "fakeclaude-stdin"))
			}
			dp, err := newDaemonProvider(filepath.Join(bin, c.provider), filepath.Join(t.TempDir(), "checkpoint.schema.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gateDaemonProvider(&dp, false, true, &host.Linux{User: "fab-worker"}); err != nil {
				t.Fatalf("the trusted pinned fake was refused: %v", err)
			}
			l := dp.Launch
			l.Prompt = "checkpoint"
			argv, _, err := claude.Argv(launchGrant(t), l)
			if err != nil {
				t.Fatalf("verified launch refused: %v", err)
			}
			// Paused after verification: the original is replaced.
			marker := filepath.Join(t.TempDir(), "replacement-ran")
			swap := filepath.Join(bin, ".swap")
			if err := os.WriteFile(swap, []byte("#!/bin/sh\ntouch '"+marker+"'\necho REPLACEMENT\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(swap, filepath.Join(bin, c.replace)); err != nil {
				t.Fatal(err)
			}
			// Resume: execute what the launch named.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, c.run(argv[0]))
			cmd.Env = []string{"PATH=/usr/bin:/bin", "FAB=/bin/false", "FAB_WORKTREE=" + t.TempDir(), "FAB_RUN_DIR=" + t.TempDir()}
			out, _ := cmd.CombinedOutput()
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the replacement ran after verification (exec %s): %s", c.run(argv[0]), out)
			}
			if !strings.Contains(string(out), `"claude_code_version":"fake-1"`) {
				t.Fatalf("the verified fake did not run: %s", out)
			}
		})
	}
}
