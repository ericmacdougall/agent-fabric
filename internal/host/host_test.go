package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGrantValidateFailsClosed(t *testing.T) {
	g := controlGrant(t.TempDir())
	if err := g.Validate(); err != nil {
		t.Fatalf("control grant should validate: %v", err)
	}
	cases := map[string]func(*Grant){
		"no model":       func(g *Grant) { g.Model = " " },
		"no effort":      func(g *Grant) { g.Effort = "" },
		"bad fence":      func(g *Grant) { g.Fence = 0 },
		"bad profile":    func(g *Grant) { g.Profile = "P2" },
		"turns too high": func(g *Grant) { g.MaxTurns = 5000 },
		"no budget":      func(g *Grant) { g.MaxBudgetUSD = 0 },
		"relative path":  func(g *Grant) { g.Worktree = "wt" },
		"missing dir":    func(g *Grant) { g.RunDir = filepath.Join(g.RunDir, "nope") },
		"short token":    func(g *Grant) { g.Token = "abc" },
		"no tier table":  func(g *Grant) { g.TierTableSHA = "" },
	}
	for name, mutate := range cases {
		m := g
		mutate(&m)
		if err := m.Validate(); !errors.Is(err, ErrGrantInvalid) {
			t.Fatalf("%s: want ErrGrantInvalid, got %v", name, err)
		}
	}
}

// The negative-control suite is the live proof of this host's boundaries. It
// spawns only processes it starts itself, bounded by the sleepers' timeouts
// and the stop grace. FABRIC_HOST_CONTROLS=1 opts in (it signals processes).
func TestNegativeControls(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run the process-signalling controls")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for the setsid escapee")
	}
	scratch := t.TempDir()
	p := testProfile(t)
	results, err := p.NegativeControls(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("only %d controls ran", len(results))
	}
	var failed []string
	for _, r := range results {
		t.Logf("%-45s %-5v %s", r.Name, r.Passed, r.Detail)
		if !r.Passed {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("controls failed on %s: %s", p.Name(), strings.Join(failed, ", "))
	}
}

// A real grant for P1 on the Mac is refused (D6) even when everything else
// is valid; the controls run P1 only through their own runner.
func TestDarwinRefusesWriterGrants(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	d := darwinProfile(t)
	g := controlGrant(t.TempDir())
	g.Profile = "P1"
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	if _, err := d.Start(g, []string{"true"}, nil, devnull, devnull); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("P1 on the Mac: want ErrGrantInvalid, got %v", err)
	}
	g.Profile = "P0"
	h, err := d.Start(g, []string{"true"}, nil, devnull, devnull)
	if err != nil {
		t.Fatalf("P0 refused: %v", err)
	}
	code, _ := d.Wait(h)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	_ = time.Second
}
