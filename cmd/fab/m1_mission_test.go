package main

import (
	"os"
	"os/user"
	"path/filepath"
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
