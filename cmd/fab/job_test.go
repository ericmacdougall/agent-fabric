package main

import (
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15082 F0: `fab job watch` and `fab job run` refuse before any
// side effect: no run directory, no command run, no database access.
func TestJobCommandsRefusedWithoutSideEffects(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://127.0.0.1:1/none?connect_timeout=1")
	dir := t.TempDir()
	rundir := filepath.Join(dir, "run")
	marker := filepath.Join(dir, "marker")
	for _, args := range [][]string{
		{"watch", "--id", "j_x", "--rundir", rundir, "--", "touch", marker},
		{"run", "--mission", "m_x", "--attempt", "r_x", "--fence", "1", "--rundir", rundir, "--", "touch", marker},
	} {
		err := jobMain(args)
		if err == nil {
			t.Errorf("job %s: accepted", args[0])
		}
		if exitCode(err) != 3 {
			t.Errorf("job %s: exit %d, want 3 (refused, nothing changed): %v", args[0], exitCode(err), err)
		}
	}
	for _, p := range []string{rundir, marker} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}
