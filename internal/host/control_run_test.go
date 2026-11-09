package host

import (
	"testing"

	"fabric/internal/domain"
)

// Every control start needs its own run id: the id names the transient unit
// and the environment file, and a reused id collides with the file the
// previous start left behind (goodqa#15466).
func TestFreshRunIDsAreDistinctAndValid(t *testing.T) {
	g := controlGrant(t.TempDir())
	seen := map[domain.AttemptID]bool{g.Run: true}
	for i := 0; i < 1000; i++ {
		f := freshRun(g)
		if seen[f.Run] {
			t.Fatalf("run id %s reused", f.Run)
		}
		seen[f.Run] = true
		if _, err := domain.ParseAttemptID(string(f.Run)); err != nil {
			t.Fatalf("run id %q is not a valid attempt id: %v", f.Run, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("grant with a fresh run id is invalid: %v", err)
		}
		same := f
		same.Run = g.Run
		if same != g {
			t.Fatal("freshRun changed more than the run id")
		}
	}
}
