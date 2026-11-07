//go:build linux

package host

import (
	"errors"
	"strings"
	"testing"
)

// goodqa#15087 AC-6 (a): every name fabric sets gets exactly one -E with its
// final value (a caller pair replaces fabric's default), and --env-keep
// lists exactly those names, so the worker sees nothing else. A variable
// only in fabd's own environment gets no -E.
func TestLinuxEnvOverrideAndKeep(t *testing.T) {
	t.Setenv("FABRIC_SENTINEL_ENV", "planted")
	t.Setenv("ANTHROPIC_API_KEY", "planted-key")
	g := controlGrant(t.TempDir())
	l := &Linux{Self: "/srv/fabric/bin/fab"}
	args, err := l.startArgs(g, "fab-x.service", []string{"env"}, []string{"LANG=fabric-lang", "CLAUDE_CONFIG_DIR=/run/x/claude-config", "FAB_MISSION=m_1"})
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
	want := map[string]string{"FAB_RUN": g.Token, "LANG": "fabric-lang", "CLAUDE_CONFIG_DIR": "/run/x/claude-config", "FAB_MISSION": "m_1"}
	got := map[string][]string{}
	var keep string
	sawSandbox := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-E" && i+1 < len(args) && !sawSandbox:
			name, val, _ := strings.Cut(args[i+1], "=")
			got[name] = append(got[name], val)
			i++
		case args[i] == "__sandbox":
			sawSandbox = true
		case args[i] == "--env-keep" && i+1 < len(args):
			keep = args[i+1]
			i++
		}
	}
	// Every allow-listed name fabric leaves unset is overridden to empty,
	// so the manager's value cannot reach the sandbox; --env-keep (below)
	// then drops it.
	for _, n := range WorkerEnvNames {
		if _, set := want[n]; set || n == "HOME" || n == "PATH" {
			continue
		}
		if len(got[n]) != 1 || got[n][0] != "" {
			t.Fatalf("-E for unset allow-listed %s: %q, want exactly one empty override", n, got[n])
		}
		delete(got, n)
	}
	for n, v := range want {
		if len(got[n]) != 1 || got[n][0] != v {
			t.Fatalf("-E for %s: %q, want exactly one with the final value", n, got[n])
		}
	}
	for _, n := range []string{"HOME", "PATH"} {
		if len(got[n]) != 1 {
			t.Fatalf("-E for %s appears %d times, want 1", n, len(got[n]))
		}
	}
	for _, n := range []string{"FABRIC_SENTINEL_ENV", "ANTHROPIC_API_KEY"} {
		if _, ok := got[n]; ok {
			t.Fatalf("fabd's own %s was passed to the worker", n)
		}
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	if keep == "" {
		t.Fatal("no --env-keep: the worker would inherit the systemd manager's environment")
	}
	keepSet := map[string]bool{}
	for _, n := range strings.Split(keep, ",") {
		keepSet[n] = true
	}
	if len(keepSet) != len(names) {
		t.Fatalf("--env-keep %q, want exactly the %d names given -E (%v)", keep, len(names), names)
	}
	for _, n := range names {
		if !keepSet[n] {
			t.Fatalf("--env-keep %q misses %s", keep, n)
		}
	}
	if _, err := l.startArgs(g, "fab-x.service", []string{"env"}, []string{"NOT A NAME=x"}); err == nil {
		t.Fatal("a malformed environment entry was accepted")
	}
	for _, e := range []string{"LD_PRELOAD=/x.so", "ANTHROPIC_API_KEY=k", "BASH_ENV=/x"} {
		if _, err := l.startArgs(g, "fab-x.service", []string{"env"}, []string{e}); !errors.Is(err, ErrGrantInvalid) {
			t.Fatalf("%s outside the allow-list: %v, want ErrGrantInvalid", e, err)
		}
	}
}
