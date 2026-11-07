//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fabric/internal/host"
)

// goodqa#15087 AC-6 (a) and goodqa#15177: `fab __sandbox` execs the worker
// with exactly the pairs of its --env-file, which must name exactly the
// --env-keep names. Whatever the worker user's systemd manager planted (a
// sentinel, a key, LD_PRELOAD) never reaches the worker, because the
// environment no longer comes from environ at all. The file is removed
// once read. Without both flags the launch is refused.
func TestSandboxExecEnvExact(t *testing.T) {
	t.Setenv("FABRIC_SENTINEL_ENV", "planted")
	t.Setenv("LD_PRELOAD", "/nonexistent/fabric-sentinel.so")
	dir := t.TempDir()
	n := 0
	write := func(pairs ...string) string {
		n++
		p := filepath.Join(dir, fmt.Sprintf("env-%d", n))
		if err := host.WriteEnvFile(p, pairs, false); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := write("FAB_RUN=tok", "HOME=/srv/fabric/home", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "FABRIC_DSN=a=b c$HOME\\x")
	args := []string{"--env-keep", "FABRIC_DSN,FAB_RUN,HOME,LANG,PATH", "--env-file", f, "--ro", "/usr", "--rw", "/tmp/run", "--tcp", "443", "--", "env", "-0"}
	sb, argv, env, err := sandboxPlan(args)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(env)
	want := []string{"FABRIC_DSN=a=b c$HOME\\x", "FAB_RUN=tok", "HOME=/srv/fabric/home", "LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("worker env %q, want exactly %q", env, want)
	}
	if !reflect.DeepEqual(argv, []string{"env", "-0"}) || len(sb.ReadOnly) != 1 || len(sb.ReadWrite) != 1 || len(sb.TCPPorts) != 1 {
		t.Fatalf("argv %q, sandbox %+v", argv, sb)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Fatalf("environment file still present after read: %v", err)
	}

	link := filepath.Join(dir, "link")
	target := write("FAB_RUN=tok")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]string{
		"no --env-keep":        {"--env-file", write("FAB_RUN=tok"), "--", "env"},
		"no --env-file":        {"--env-keep", "FAB_RUN", "--", "env"},
		"not a name":           {"--env-keep", "FAB_RUN,A B", "--env-file", write("FAB_RUN=tok"), "--", "env"},
		"empty keep":           {"--env-keep", "", "--env-file", write("FAB_RUN=tok"), "--", "env"},
		"no value":             {"--env-keep"},
		"entry not kept":       {"--env-keep", "FAB_RUN", "--env-file", write("FAB_RUN=tok", "HOME=/h"), "--", "env"},
		"kept name missing":    {"--env-keep", "FAB_RUN,HOME", "--env-file", write("FAB_RUN=tok"), "--", "env"},
		"duplicate entry":      {"--env-keep", "FAB_RUN", "--env-file", write("FAB_RUN=a", "FAB_RUN=b"), "--", "env"},
		"off the allow-list":   {"--env-keep", "LD_PRELOAD", "--env-file", write("LD_PRELOAD=/x.so"), "--", "env"},
		"symlinked file":       {"--env-keep", "FAB_RUN", "--env-file", link, "--", "env"},
		"missing file":         {"--env-keep", "FAB_RUN", "--env-file", filepath.Join(dir, "absent"), "--", "env"},
		"entry without equals": {"--env-keep", "FAB_RUN", "--env-file", write("FAB_RUN"), "--", "env"},
	} {
		if _, _, _, err := sandboxPlan(bad); err == nil {
			t.Errorf("%s: sandboxPlan(%q) accepted", name, strings.Join(bad, " "))
		}
	}
}
