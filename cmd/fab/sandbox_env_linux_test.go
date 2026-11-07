//go:build linux

package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// goodqa#15087 AC-6 (a): `fab __sandbox` execs the worker with exactly the
// --env-keep names of its environment. What the worker user's systemd
// manager planted (a sentinel, a key, LD_PRELOAD, a LANG override that was
// not re-set) is dropped; without --env-keep the launch is refused.
func TestSandboxExecEnvExact(t *testing.T) {
	environ := []string{
		"FAB_RUN=tok", "HOME=/srv/fabric/home", "PATH=/usr/bin:/bin", "LANG=C.UTF-8",
		"FABRIC_SENTINEL_ENV=planted", "ANTHROPIC_API_KEY=planted-key", "LD_PRELOAD=/nonexistent/fabric-sentinel.so",
		"MANAGERPATH=x", "INVOCATION_ID=abc",
	}
	args := []string{"--env-keep", "FAB_RUN,HOME,LANG,PATH", "--ro", "/usr", "--rw", "/tmp/run", "--tcp", "443", "--", "env", "-0"}
	sb, argv, env, err := sandboxPlan(args, environ)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(env)
	want := []string{"FAB_RUN=tok", "HOME=/srv/fabric/home", "LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("worker env %q, want exactly %q", env, want)
	}
	if !reflect.DeepEqual(argv, []string{"env", "-0"}) || len(sb.ReadOnly) != 1 || len(sb.ReadWrite) != 1 || len(sb.TCPPorts) != 1 {
		t.Fatalf("argv %q, sandbox %+v", argv, sb)
	}
	// A name that is kept but not present stays absent (no empty entry).
	_, _, env, err = sandboxPlan([]string{"--env-keep", "FAB_RUN,NOT_SET", "--", "env"}, environ)
	if err != nil || len(env) != 1 || env[0] != "FAB_RUN=tok" {
		t.Fatalf("keep with an unset name: %q %v", env, err)
	}
	for _, bad := range [][]string{
		{"--", "env"}, // no --env-keep
		{"--env-keep", "FAB_RUN,A B", "--", "env"}, // not a name
		{"--env-keep", "", "--", "env"},            // empty list
		{"--env-keep"},                             // no value
	} {
		if _, _, _, err := sandboxPlan(bad, environ); err == nil {
			t.Fatalf("sandboxPlan(%q) accepted", strings.Join(bad, " "))
		}
	}
}
