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
	}{
		{"recorded fake replaced", "fakeclaude-live", "fakeclaude-live"},
		{"stdin fake's delegate replaced", "fakeclaude-stdin", "fakeclaude-live"},
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
			// Resume: execute what the launch named (the wrapper itself
			// for the stdin fake, fed one stream-json message).
			out := runLaunched(t, argv[0], c.provider == "fakeclaude-stdin")
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the replacement ran after verification (exec %s): %s", argv[0], out)
			}
			if !strings.Contains(out, `"claude_code_version":"fake-1"`) {
				t.Fatalf("the verified fake did not run: %s", out)
			}
		})
	}
}

// goodqa#15976 delta review F2: the sealed wrapper must find the sealed
// recorded fake whatever the input files were called, including names that
// collide with the wrapper's own delegate lookup (fakeclaude-live, then
// fakeclaude.sh). Each layout runs through the wrapper itself.
func TestSealedStdinFakeFindsItsDelegateWhateverTheNames(t *testing.T) {
	fake := filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh")
	stdinFake := filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh")
	for _, c := range []struct{ wrapper, delegate string }{
		{"fakeclaude-live", "fakeclaude.sh"},
		{"fakeclaude.sh", "fakeclaude-live"},
		{"fakeclaude-stdin", "fakeclaude-live"},
	} {
		t.Run(c.wrapper+" with "+c.delegate, func(t *testing.T) {
			bin := t.TempDir()
			copyFile(t, fake, filepath.Join(bin, c.delegate))
			copyFile(t, stdinFake, filepath.Join(bin, c.wrapper))
			dp, err := newDaemonProvider(filepath.Join(bin, c.wrapper), filepath.Join(t.TempDir(), "checkpoint.schema.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gateDaemonProvider(&dp, false, true, &host.Linux{User: "fab-worker"}); err != nil {
				t.Fatalf("the trusted stdin fake was refused: %v", err)
			}
			l := dp.Launch
			l.Prompt = "checkpoint"
			argv, _, err := claude.Argv(launchGrant(t), l)
			if err != nil {
				t.Fatalf("verified launch refused: %v", err)
			}
			out := runLaunched(t, argv[0], true)
			if !strings.Contains(out, `"claude_code_version":"fake-1"`) {
				t.Fatalf("the sealed wrapper did not reach the recorded fake: %s", out)
			}
			// Fixed sealed names, whatever the inputs were called.
			ents, err := os.ReadDir(filepath.Dir(argv[0]))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			if filepath.Base(argv[0]) != "fakeclaude-stdin" || strings.Join(names, ",") != "fakeclaude-live,fakeclaude-stdin" {
				t.Fatalf("sealed %s as %v; want fakeclaude-stdin beside fakeclaude-live only", argv[0], names)
			}
		})
	}
}

// runLaunched executes argv0 as fabd's launch would. For the stdin fake it
// passes --input-format stream-json, FAB_MISSION and exactly one user
// message whose content is this mission's projection.
func runLaunched(t *testing.T, argv0 string, stdin bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	env := []string{"PATH=/usr/bin:/bin", "FAB=/bin/false", "FAB_WORKTREE=" + t.TempDir(), "FAB_RUN_DIR=" + t.TempDir()}
	if stdin {
		cmd = exec.CommandContext(ctx, argv0, "--input-format", "stream-json", "--output-format", "stream-json")
		env = append(env, "FAB_MISSION=m_seal_test")
		cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"{\"checkpoint\":{\"mission\":\"m_seal_test\"}}"}}` + "\n")
	} else {
		cmd = exec.CommandContext(ctx, argv0)
	}
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out)
}
