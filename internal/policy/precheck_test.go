package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15283 AC-5: a failing Precheck denies before any hook runs, in
// Check and so in SelfTest; a passing one leaves the hooks in charge.
func TestOraclePrecheckRunsBeforeEveryHook(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hook-ran")
	hook := []string{"sh", "-c", "touch '" + marker + "'"}
	moved := errors.New("worktree moved: /srv/wt is now another directory")
	o := &Oracle{Hooks: [][]string{hook}, Cwd: dir, Precheck: func() error { return moved }}
	d := o.Check(context.Background(), "shell", json.RawMessage(`{"command":"ls"}`))
	if d.Allow || !strings.Contains(d.Reason, moved.Error()) {
		t.Fatalf("decision %+v, want a denial naming %q", d, moved)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran although the precheck failed")
	}
	if err := o.SelfTest(context.Background()); err == nil {
		t.Fatal("SelfTest passed with a failing precheck")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SelfTest ran a hook although the precheck failed")
	}
	// Control: a passing precheck lets the hook run and decide.
	o.Precheck = func() error { return nil }
	if d := o.Check(context.Background(), "shell", json.RawMessage(`{"command":"ls"}`)); !d.Allow {
		t.Fatalf("control: %+v", d)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control: the hook did not run")
	}
}
