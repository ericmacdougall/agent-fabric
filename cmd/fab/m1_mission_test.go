package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"fabric/internal/m1build"
	"fabric/internal/mission"
)

// AC-22: only an M1 build reads mission.json; run with and without -tags m1.
func TestM1MissionPathFollowsBuild(t *testing.T) {
	if m1Enabled != m1build.Enabled {
		t.Fatal("the daemon wiring does not read m1build.Enabled")
	}
	// Disabled: nil, and nothing is checked or read, even with bad inputs.
	p, err := m1MissionPreparer(m1MissionOptions{enabled: false, missionsRoot: "/nonexistent", profile: "P1"})
	if p != nil || err != nil {
		t.Fatalf("non-M1 build: %v, %v; want nil, nil", p, err)
	}
}

func m1Opts(t *testing.T) m1MissionOptions {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	self := filepath.Join(dir, "fab")
	os.WriteFile(self, []byte("sealed fab"), 0o755)
	hooks := filepath.Join(dir, "hooks")
	os.Mkdir(hooks, 0o755)
	if err := os.Link(self, filepath.Join(hooks, mission.BashGateName)); err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("current group has no name")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("no nobody account")
	}
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0o755)
	return m1MissionOptions{enabled: true, workRoot: work, missionsRoot: filepath.Join(dir, "missions"), fabSecretGroup: g.Name,
		workerUser: "nobody", profile: "P0", hooksDir: hooks, self: self}
}

// AC-24, AC-25, R3: an M1 build refuses to start on P1, without a worker
// account, or with a gate binary that is not the running fab.
func TestM1MissionPathStartRefusals(t *testing.T) {
	if _, err := os.Stat(m1GitPath); err != nil {
		t.Skip("no " + m1GitPath)
	}
	o := m1Opts(t)
	p, err := m1MissionPreparer(o)
	if err != nil || p == nil {
		t.Fatalf("valid M1 options: %v", err)
	}
	if p.AttemptRoot != filepath.Join(o.workRoot, "attempts") || p.Git != m1GitPath || len(p.GateSHA) != 64 {
		t.Fatalf("preparer %+v", p)
	}
	for name, mutate := range map[string]func(o *m1MissionOptions){
		"profile P1":        func(o *m1MissionOptions) { o.profile = "P1" },
		"no worker":         func(o *m1MissionOptions) { o.workerUser = "" },
		"unknown worker":    func(o *m1MissionOptions) { o.workerUser = "fab-no-such-user-4242" },
		"unknown group":     func(o *m1MissionOptions) { o.fabSecretGroup = "fab-no-such-group-4242" },
		"relative hooks":    func(o *m1MissionOptions) { o.hooksDir = "hooks" },
		"swapped gate":      func(o *m1MissionOptions) { os.Remove(o.self); os.WriteFile(o.self, []byte("another build"), 0o755) },
		"worker = operator": func(o *m1MissionOptions) { u, _ := user.Current(); o.workerUser = u.Username },
	} {
		o := m1Opts(t)
		mutate(&o)
		if p, err := m1MissionPreparer(o); err == nil || p != nil {
			t.Errorf("%s: started (%v)", name, err)
		} else if !strings.Contains(err.Error(), "goodqa#15966") {
			t.Errorf("%s: refusal does not name the ticket: %v", name, err)
		}
	}
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// m1RefusesP1Dispatch: in an M1 build a dispatching daemon on profile P1
// is refused at start, before the store is opened (goodqa#15966 R3), so
// tests that expect the later store error assert that refusal instead. It
// reports whether it handled err.
func m1RefusesP1Dispatch(t *testing.T, err error) bool {
	t.Helper()
	if !m1Enabled {
		return false
	}
	if err == nil || !strings.Contains(err.Error(), "--profile must be P0") || !strings.Contains(err.Error(), "goodqa#15966") {
		t.Fatalf("M1 build: want the P0 start refusal first, got %v", err)
	}
	return true
}

// AC-24 (owner ruling): fab run under the name fab-bash-gate is the Bash
// PreToolUse gate. Hook contract (code.claude.com/docs/en/hooks): exit 0
// with no output lets the call continue to the permission rules; exit 2
// blocks it, with the reason on stderr. A name that only starts with
// fab-bash-gate is ordinary fab.
func TestBashGateArgv0Dispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fab binary")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	fab := filepath.Join(dir, "fab")
	args := []string{"build", "-o", fab}
	if m1Enabled {
		args = append(args, "-tags", "m1")
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), append(args, ".")...)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	gate := filepath.Join(dir, mission.BashGateName)
	prefix := filepath.Join(dir, mission.BashGateName+"-x")
	for _, l := range []string{gate, prefix} {
		if err := os.Link(fab, l); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(cmd string) string {
		b, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": dir, "hook_event_name": "PreToolUse", "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120000, "run_in_background": false}, "tool_use_id": "toolu_x"})
		return string(b)
	}
	run := func(bin, stdin string) (code int, stdout, stderr string) {
		var o, e bytes.Buffer
		c := exec.Command(bin)
		c.Stdin, c.Stdout, c.Stderr = strings.NewReader(stdin), &o, &e
		c.Env = []string{"PATH=/usr/bin:/bin"}
		err := c.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			code = ee.ExitCode()
		default:
			t.Fatalf("%s: %v", bin, err)
		}
		return code, o.String(), e.String()
	}
	if code, out, errs := run(gate, hook("./test.sh")); code != 0 || out != "" || errs != "" {
		t.Errorf("./test.sh: exit %d stdout %q stderr %q; want 0 and no output", code, out, errs)
	}
	for _, cmd := range []string{"ls", "./test.sh; id", "env"} {
		if code, out, errs := run(gate, hook(cmd)); code != 2 || out != "" || !strings.HasPrefix(errs, "fab-bash-gate: refused: ") {
			t.Errorf("%q: exit %d stdout %q stderr %q; want exit 2, no stdout, a refusal on stderr", cmd, code, out, errs)
		}
	}
	if code, _, errs := run(gate, "{not json"); code != 2 || !strings.Contains(errs, "refused") {
		t.Errorf("malformed input: exit %d stderr %q", code, errs)
	}
	// Broken variant: a basename prefix is not the gate. It is plain fab,
	// which prints its usage and never decides a hook call.
	code, out, errs := run(prefix, hook("./test.sh"))
	if strings.Contains(errs, "fab-bash-gate:") || strings.Contains(out, "fab-bash-gate:") || code == 0 {
		t.Errorf("%s acted as the gate: exit %d stdout %q stderr %q", filepath.Base(prefix), code, out, errs)
	}
}
