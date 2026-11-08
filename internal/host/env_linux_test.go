//go:build linux

package host

import (
	"errors"
	"strings"
	"testing"
)

// goodqa#15087 AC-6 (a), goodqa#15177: every name fabric sets appears once
// in the environment-file pairs with its final value (a caller pair
// replaces fabric's default), --env-keep lists exactly those names and
// --env-file names the run's file, so the worker sees nothing else. Every
// allow-listed name is blanked with an empty -E, so no value the manager
// holds survives and no value is on the command line. A variable only in
// fabd's own environment is in neither.
func TestLinuxEnvOverrideAndKeep(t *testing.T) {
	t.Setenv("FABRIC_SENTINEL_ENV", "planted")
	t.Setenv("ANTHROPIC_API_KEY", "planted-key")
	g := controlGrant(t.TempDir())
	l := &Linux{Self: "/srv/fabric/bin/fab"}
	args, pairs, err := l.startArgs(g, "fab-x.service", envFile(g), []string{"env"}, []string{"LANG=fabric-lang", "CLAUDE_CONFIG_DIR=/run/x/claude-config", "FAB_MISSION=m_1"})
	if err != nil {
		t.Fatal(err)
	}
	// Inspection 6355639 F1: without --expand-environment=no the manager
	// substitutes $VAR in the worker argv from its own environment.
	expandOff := false
	for _, a := range args {
		if a == "__sandbox" {
			break
		}
		if a == "--expand-environment=no" {
			expandOff = true
		}
	}
	if !expandOff {
		t.Fatalf("systemd-run args %q lack --expand-environment=no before the command", args)
	}
	blank := map[string]int{}
	var keep, file string
	sawSandbox := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-E" && i+1 < len(args) && !sawSandbox:
			name, val, _ := strings.Cut(args[i+1], "=")
			if val != "" {
				t.Fatalf("-E %s carries a value on the command line", name)
			}
			blank[name]++
			i++
		case args[i] == "__sandbox":
			sawSandbox = true
		case args[i] == "--env-keep" && i+1 < len(args):
			keep = args[i+1]
			i++
		case args[i] == "--env-file" && i+1 < len(args):
			file = args[i+1]
			i++
		}
	}
	for _, n := range WorkerEnvNames {
		if blank[n] != 1 {
			t.Fatalf("-E %s= appears %d times, want exactly one blanking override", n, blank[n])
		}
	}
	if len(blank) != len(WorkerEnvNames) {
		t.Fatalf("%d names blanked, want the %d allow-listed ones", len(blank), len(WorkerEnvNames))
	}
	if file != envFile(g) {
		t.Fatalf("--env-file %q, want %q", file, envFile(g))
	}
	got := map[string][]string{}
	for _, p := range pairs {
		name, val, _ := strings.Cut(p, "=")
		got[name] = append(got[name], val)
	}
	want := map[string]string{"FAB_RUN": g.Token, "LANG": "fabric-lang", "CLAUDE_CONFIG_DIR": "/run/x/claude-config", "FAB_MISSION": "m_1"}
	for n, v := range want {
		if len(got[n]) != 1 || got[n][0] != v {
			t.Fatalf("pair for %s: %q, want exactly one with the final value", n, got[n])
		}
	}
	for _, n := range []string{"HOME", "PATH"} {
		if len(got[n]) != 1 {
			t.Fatalf("pair for %s appears %d times, want 1", n, len(got[n]))
		}
	}
	for _, n := range []string{"FABRIC_SENTINEL_ENV", "ANTHROPIC_API_KEY"} {
		if _, ok := got[n]; ok {
			t.Fatalf("fabd's own %s was passed to the worker", n)
		}
	}
	if keep == "" {
		t.Fatal("no --env-keep: the worker environment is not named")
	}
	keepSet := map[string]bool{}
	for _, n := range strings.Split(keep, ",") {
		keepSet[n] = true
	}
	if len(keepSet) != len(got) {
		t.Fatalf("--env-keep %q, want exactly the %d names in the file", keep, len(got))
	}
	for n := range got {
		if !keepSet[n] {
			t.Fatalf("--env-keep %q misses %s", keep, n)
		}
	}
	if _, _, err := l.startArgs(g, "fab-x.service", envFile(g), []string{"env"}, []string{"NOT A NAME=x"}); err == nil {
		t.Fatal("a malformed environment entry was accepted")
	}
	for _, e := range []string{"LD_PRELOAD=/x.so", "ANTHROPIC_API_KEY=k", "BASH_ENV=/x"} {
		if _, _, err := l.startArgs(g, "fab-x.service", envFile(g), []string{"env"}, []string{e}); !errors.Is(err, ErrGrantInvalid) {
			t.Fatalf("%s outside the allow-list: %v, want ErrGrantInvalid", e, err)
		}
	}
}
