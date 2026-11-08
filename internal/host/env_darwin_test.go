//go:build darwin

package host

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// goodqa#15087 AC-6 (b): a Darwin worker's environment is exactly fabric's
// pairs. A sentinel and a fake key planted in this process's environment do
// not reach it; a caller pair replaces fabric's default. The env is then
// run through a real exec (env -0) to prove exec.Cmd passes nothing else.
func TestDarwinEnvExact(t *testing.T) {
	t.Setenv("FABRIC_SENTINEL_ENV", "planted")
	t.Setenv("ANTHROPIC_API_KEY", "planted-key")
	g := controlGrant(t.TempDir())
	env, err := darwinEnv(g, []string{"LANG=fabric-lang", "FAB_MISSION=m_1"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/env", "-0")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	got := map[string]string{}
	for _, kv := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		n, v, _ := strings.Cut(kv, "=")
		names = append(names, n)
		got[n] = v
	}
	sort.Strings(names)
	want := []string{"FAB_MISSION", "FAB_RUN", "HOME", "LANG", "PATH", "SHELL", "TMPDIR", "USER"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("worker env names %v, want exactly %v", names, want)
	}
	if got["LANG"] != "fabric-lang" || got["FAB_RUN"] != g.Token || got["HOME"] != os.Getenv("HOME") {
		t.Fatalf("worker env values wrong: LANG=%q FAB_RUN matches=%v", got["LANG"], got["FAB_RUN"] == g.Token)
	}
}
