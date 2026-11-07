package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// providerTree is an installer layout: two version files and a bin symlink
// pointing at the first.
func providerTree(t *testing.T) (bin, link, v1, v2 string) {
	t.Helper()
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions")
	bin = filepath.Join(dir, "bin")
	for _, d := range []string{versions, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	v1 = filepath.Join(versions, "1.0.0")
	v2 = filepath.Join(versions, "2.0.0")
	for _, v := range []string{v1, v2} {
		if err := os.WriteFile(v, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link = filepath.Join(bin, "provider-k3")
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}
	return bin, link, v1, v2
}

func realPathT(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// goodqa#15161 AC-R1 (review R1 on 707338d): `fab daemon` with a provider
// that does not resolve to a file refuses to start with a named cause,
// before it writes anything, opens a store or runs a control, with or
// without --trust-provider. Before, the provider control was silently
// skipped (it had no file to run), containment passed and fabd started.
func TestDaemonRefusesUnresolvableProvider(t *testing.T) {
	t.Setenv("FABRIC_DSN", "") // a refusal that reached the store would name the DSN instead
	tiers := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(tiers, []byte(`{"rungs":{"r1":{"effort":"medium","models":{"claude":"m"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink: the bin entry is there, its version was pruned.
	dangling := filepath.Join(t.TempDir(), "provider-k3")
	if err := os.Symlink(filepath.Join(t.TempDir(), "pruned"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ what, provider string }{
		{"missing name", "no-such-provider-k3-15161"},
		{"dangling symlink", dangling},
	} {
		for _, trust := range []bool{true, false} {
			workRoot := filepath.Join(t.TempDir(), "wr")
			args := []string{"--work-root", workRoot, "--provider", c.provider, "--tier-table", tiers, "--rung", "r1", "--once"}
			if trust {
				args = append(args, "--trust-provider")
			}
			err := daemonMain(args)
			want := `provider "` + c.provider + `" cannot be resolved`
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "refusing to start") {
				t.Errorf("%s, trust=%v: daemon err = %v, want a start refusal naming %s", c.what, trust, err, want)
			}
			if _, statErr := os.Stat(workRoot); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("%s, trust=%v: the work root was created before the refusal (%v)", c.what, trust, statErr)
			}
		}
	}
}

// goodqa#15161 AC-4: the start-time resolution is the file the provider
// control runs; a name that does not resolve is an error, never "".
func TestResolveStartProvider(t *testing.T) {
	bin, link, v1, _ := providerTree(t)
	if p, err := resolveStartProvider(link); err != nil || p != realPathT(t, v1) {
		t.Errorf("resolveStartProvider(%s) = %q, %v; want %q", link, p, err, realPathT(t, v1))
	}
	t.Setenv("PATH", bin)
	if p, err := resolveStartProvider("provider-k3"); err != nil || p != realPathT(t, v1) {
		t.Errorf("resolveStartProvider(name on PATH) = %q, %v; want %q", p, err, realPathT(t, v1))
	}
	if p, err := resolveStartProvider("no-such-provider-k3-15161"); err == nil || p != "" {
		t.Errorf("unresolvable provider: %q, %v; want an error and no path", p, err)
	}
}

func launchGrant(t *testing.T) host.Grant {
	t.Helper()
	dir := t.TempDir()
	return host.Grant{Run: "r_1", Mission: "m_1", Fence: 1, Model: "policy-model-x", Effort: "medium", Rung: "R0",
		TierTableSHA: "abc", Profile: "P1", MaxTurns: 40, MaxBudgetUSD: 2, Wall: 45 * time.Minute,
		Worktree: dir, RunDir: dir, Token: "0123456789abcdef0123"}
}

// goodqa#15161 AC-N1 and review R3: the launch fabd hands the scheduler
// resolves the provider at every launch. After an update repoints the
// symlink and prunes the old version, the next argv names the new file;
// argv[0] is never the operator's name or the symlink; and a provider
// that no longer resolves refuses the launch with a named cause.
func TestProductionLaunchResolvesPerLaunch(t *testing.T) {
	bin, link, v1, v2 := providerTree(t)
	t.Setenv("PATH", bin)
	g := launchGrant(t)
	for _, name := range []string{"provider-k3", link} {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.WriteFile(v1, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(v1, link); err != nil {
			t.Fatal(err)
		}
		l := productionLaunch(name, "/srv/wr/checkpoint.schema.json", "")
		l.Prompt = "checkpoint"
		if l.Binary != name {
			t.Fatalf("%s: Binary = %q, want the operator's name (pinning it is N1)", name, l.Binary)
		}
		argv, _, err := claude.Argv(g, l)
		if err != nil || argv[0] != realPathT(t, v1) {
			t.Fatalf("%s: first launch argv[0] = %q, err %v; want %q", name, first(argv), err, realPathT(t, v1))
		}
		// The update: repoint, then prune the old version.
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(v2, link); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(v1); err != nil {
			t.Fatal(err)
		}
		argv, _, err = claude.Argv(g, l)
		if err != nil || argv[0] != realPathT(t, v2) {
			t.Fatalf("%s: launch after the update argv[0] = %q, err %v; want %q", name, first(argv), err, realPathT(t, v2))
		}
		// The provider is gone: a named refusal, no stale fallback.
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		argv, _, err = claude.Argv(g, l)
		if err == nil || argv != nil || !errors.Is(err, host.ErrGrantInvalid) || !strings.Contains(err.Error(), "cannot be resolved") {
			t.Fatalf("%s: removed provider: argv %v, err %v; want a named refusal", name, argv, err)
		}
	}
}

func first(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

// goodqa#15161 AC-R4: when LookPath finds the symlink but it cannot be
// resolved (an updater removed it in between), providerPath is "", never
// the symlink path, so the launch refuses instead of exec'ing through a
// symlink in a directory the Darwin profile denies.
func TestProviderPathRefusesUnresolvableSymlink(t *testing.T) {
	_, link, v1, _ := providerTree(t)
	if p := providerPath(link); p != realPathT(t, v1) {
		t.Fatalf("providerPath(%s) = %q, want %q", link, p, realPathT(t, v1))
	}
	orig := evalSymlinks
	t.Cleanup(func() { evalSymlinks = orig })
	evalSymlinks = func(p string) (string, error) {
		os.Remove(link) // the updater's swap lands between LookPath and here
		return orig(p)
	}
	if p := providerPath(link); p != "" {
		t.Errorf("providerPath with an unresolvable symlink = %q, want \"\"", p)
	}
	// A dangling symlink resolves to nothing too.
	evalSymlinks = orig
	if err := os.Symlink(filepath.Join(filepath.Dir(v1), "pruned"), link); err != nil {
		t.Fatal(err)
	}
	if p := providerPath(link); p != "" {
		t.Errorf("providerPath with a dangling symlink = %q, want \"\"", p)
	}
}
