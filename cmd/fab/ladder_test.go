package main

import "testing"

// The ladder logic is tested in internal/ladder (no database). This only
// checks the CLI's refusals, which need no store either.
func TestLadderMainUsageIsRefusal(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"status"}, {"status", "--state", "x"}, {"status", "--state", "x", "--policy", "y", "extra"}} {
		if err := ladderMain(args); exitCode(err) != 3 {
			t.Errorf("args %q: exit %d (%v), want 3", args, exitCode(err), err)
		}
	}
}
