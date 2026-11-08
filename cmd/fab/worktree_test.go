package main

import (
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15283 NEG-7: `fab attempt start --lock-path` needs --worktree, valid
// and already canonical; the CLI refuses before it opens a store (exit 3).
func TestAttemptStartWorktree(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://refused.invalid/none") // reached only by the control
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "wt")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	base := []string{"start", "--mission", "m_x", "--id", "r_x", "--key", "k"}
	native := append(append([]string{}, base...), "--path", "anthropic-api", "--lock-path", "/s/native-locks/r_x.lock")
	for _, c := range []struct {
		what string
		args []string
	}{
		{"no --worktree", native},
		{"relative --worktree", append(append([]string{}, native...), "--worktree", "srv/wt")},
		{"--worktree with a .. segment", append(append([]string{}, native...), "--worktree", wt+"/../wt")},
		{"--worktree of /", append(append([]string{}, native...), "--worktree", "/")},
		{"non-canonical --worktree (a symlink)", append(append([]string{}, native...), "--worktree", link)},
		{"missing --worktree directory", append(append([]string{}, native...), "--worktree", filepath.Join(dir, "missing"))},
		{"claude-code with a worktree", append(append([]string{}, base...), "--path", "claude-code", "--worktree", wt)},
	} {
		if code := exitCode(attemptMain(c.args)); code != 3 {
			t.Errorf("attempt start, %s: exit %d, want 3", c.what, code)
		}
	}
	// Control: a canonical worktree passes the CLI checks and reaches the
	// store, which is unreachable here (not a usage refusal).
	err = attemptMain(append(append([]string{}, native...), "--worktree", wt))
	if err == nil || exitCode(err) == 3 {
		t.Fatalf("canonical --worktree: %v (exit %d), want past the CLI checks", err, exitCode(err))
	}
}
