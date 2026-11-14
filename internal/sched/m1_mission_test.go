package sched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fabric/internal/checkout"
	"fabric/internal/domain"
	"fabric/internal/mission"
	"fabric/internal/storetest"
)

// m1Rig is an M1 mission path on real temp directories: a bare mirror, a
// mission dir with mission.json (its real gid stands in for fab-secret), a
// hooks dir with the gate, and an operator-owned attempt root.
type m1Rig struct {
	base, missions, sha string
	prep                *mission.Preparer
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newM1Rig(t *testing.T) *m1Rig {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	gitBin, _ = filepath.EvalSymlinks(gitBin)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	t.Cleanup(func() { checkout.RemoveTree(base) })
	r := &m1Rig{base: base, missions: filepath.Join(base, "missions")}
	src := filepath.Join(base, "src")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "test.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	runGitT(t, base, "init", "-q", src)
	runGitT(t, src, "add", ".")
	runGitT(t, src, "commit", "-q", "-m", "canary")
	r.sha = runGitT(t, src, "rev-parse", "HEAD")
	runGitT(t, base, "clone", "-q", "--bare", src, filepath.Join(base, "mirror.git"))
	hooks := filepath.Join(base, "hooks")
	os.Mkdir(hooks, 0o755)
	body := []byte("sealed fab")
	os.WriteFile(filepath.Join(hooks, mission.BashGateName), body, 0o755)
	sum := sha256.Sum256(body)
	os.Mkdir(r.missions, 0o755)
	os.WriteFile(filepath.Join(base, "brief.md"), []byte("brief\n"), 0o644)
	attempts := filepath.Join(base, "attempts")
	os.Mkdir(attempts, 0o755)
	fi, _ := os.Stat(base)
	gid := fi.Sys().(*syscall.Stat_t).Gid
	r.prep = &mission.Preparer{
		Loader: mission.Loader{Root: r.missions, Stat: checkout.OS{}, FabSecretGID: gid,
			Who: checkout.Principals{OperatorUID: uint32(os.Getuid()), WorkerUID: 4242001, WorkerGIDs: []uint32{4242002}}},
		Git: gitBin, AttemptRoot: attempts, HooksDir: hooks, GateSHA: hex.EncodeToString(sum[:]),
	}
	return r
}

func (r *m1Rig) writeConfig(t *testing.T, id domain.MissionID, caps map[string]any) {
	t.Helper()
	dir := filepath.Join(r.missions, string(id))
	os.Mkdir(dir, 0o755)
	doc := map[string]any{"version": 1, "issue_id": "0b6f7c1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f", "brief": filepath.Join(r.base, "brief.md"),
		"repo": filepath.Join(r.base, "mirror.git"), "sha": r.sha, "caps": caps,
		"tools": []string{"Read", "Grep", "Glob", "Bash"}, "allow": []string{"Bash(./test.sh)"}}
	b, _ := json.Marshal(doc)
	p := filepath.Join(dir, mission.FileName)
	os.WriteFile(p, b, 0o640)
	os.Chmod(p, 0o640)
}

func m1Config(t *testing.T, st interface{}, workRoot string) Config {
	return Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P0", MaxTurns: 999,
		MaxBudgetUSD: 999, Wall: 24 * time.Hour, WorkRoot: workRoot}
}

func countRows(t *testing.T, s *Scheduler, q string, id domain.MissionID) int {
	t.Helper()
	var n int
	if err := s.st.Pool().QueryRow(context.Background(), q, string(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC-25 / NEG-19 and AC-22: a config over any ceiling, a missing or unsafe
// config, or a P1 profile on the M1 path is refused before START: no
// attempt, no new event, 0 processes.
func TestM1RefusedBeforeStartStartsNothing(t *testing.T) {
	st := storetest.Open(t)
	setCap(t, st, 20)
	r := newM1Rig(t)
	good := map[string]any{"max_turns": 40, "wall_minutes": 45, "max_budget_usd": 2}
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for kk, vv := range good {
			c[kk] = vv
		}
		c[k] = v
		return c
	}
	cases := []struct {
		id      domain.MissionID
		caps    map[string]any
		profile string
		mutate  func(path string)
	}{
		{id: "m_m1turns", caps: with("max_turns", 41)},
		{id: "m_m1wall", caps: with("wall_minutes", 46)},
		{id: "m_m1budget", caps: with("max_budget_usd", 2.01)},
		{id: "m_m1missing"},
		{id: "m_m1mode", caps: good, mutate: func(p string) { os.Chmod(p, 0o644) }},
		{id: "m_m1p1", caps: good, profile: "P1"},
	}
	for i, c := range cases {
		admitSpec(t, st, c.id, domain.ProviderClaudeCode, i+1)
		if c.caps != nil {
			r.writeConfig(t, c.id, c.caps)
		}
		if c.mutate != nil {
			c.mutate(filepath.Join(r.missions, string(c.id), mission.FileName))
		}
		ln := &fakeLauncher{st: st, block: make(chan struct{})}
		cfg := m1Config(t, st, t.TempDir())
		cfg.RunnerDSN = storetest.RunnerDSN(t, st)
		cfg.Launch.Binary = "/bin/true"
		cfg.Mission = r.prep
		if c.profile != "" {
			cfg.Profile = c.profile
		}
		s := New(st, cfg, ln)
		before := countRows(t, s, "SELECT count(*) FROM events WHERE mission_id = $1", c.id)
		err := s.runOnce(context.Background(), c.id)
		close(ln.block)
		if err == nil {
			t.Errorf("%s: runOnce accepted", c.id)
		}
		if n := countRows(t, s, "SELECT count(*) FROM attempts WHERE mission_id = $1", c.id); n != 0 {
			t.Errorf("%s: %d attempts, want 0", c.id, n)
		}
		if after := countRows(t, s, "SELECT count(*) FROM events WHERE mission_id = $1", c.id); after != before {
			t.Errorf("%s: events %d -> %d, want unchanged", c.id, before, after)
		}
		if got := atomic.LoadInt32(&ln.starts); got != 0 {
			t.Errorf("%s: %d processes started, want 0", c.id, got)
		}
		if err != nil && strings.Contains(err.Error(), r.sha) {
			t.Errorf("%s: refusal echoes the sha", c.id)
		}
		entries, _ := os.ReadDir(r.prep.AttemptRoot)
		if len(entries) != 0 {
			t.Errorf("%s: attempt dirs created: %d", c.id, len(entries))
		}
	}
}

// AC-23, AC-24, AC-25, R3: a valid config launches with its caps, the
// pinned tools, the attempt settings and the brief, from a per-attempt
// worktree outside the group-shared run dir; the per-mission 0775+setgid
// worktree is never created.
func TestM1LaunchUsesMissionPath(t *testing.T) {
	st := storetest.Open(t)
	setCap(t, st, 5)
	r := newM1Rig(t)
	const id = domain.MissionID("m_m1ok")
	admitSpec(t, st, id, domain.ProviderClaudeCode, 1)
	r.writeConfig(t, id, map[string]any{"max_turns": 12, "wall_minutes": 30, "max_budget_usd": 1.5})
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	t.Cleanup(func() { close(ln.block) })
	work, _ := filepath.EvalSymlinks(t.TempDir())
	cfg := m1Config(t, st, work)
	cfg.RunnerDSN = storetest.RunnerDSN(t, st)
	cfg.Launch.Binary = "/bin/true"
	cfg.Launch.Tools = []string{"Read", "Edit"}
	cfg.Mission = r.prep
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&ln.starts) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("never launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ln.mu.Lock()
	argv, env := ln.argvs[0], ln.envs[0]
	ln.mu.Unlock()
	attempt := "r_m1ok_1"
	dir := filepath.Join(r.prep.AttemptRoot, attempt)
	wt := filepath.Join(dir, mission.WorktreeDir)
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"--tools\x00Read,Grep,Glob,Bash", "--settings\x00" + filepath.Join(dir, mission.SettingsFile),
		"--append-system-prompt-file\x00" + filepath.Join(r.base, "brief.md"), "--permission-mode\x00dontAsk",
		"--max-turns\x0012", "--max-budget-usd\x001.50"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q", strings.ReplaceAll(want, "\x00", " "))
		}
	}
	if !contains(env, "FAB_WORKTREE="+wt) {
		t.Errorf("worker env lacks FAB_WORKTREE=%s", wt)
	}
	runDir := filepath.Join(work, "run", attempt)
	if strings.HasPrefix(wt, runDir) || strings.HasPrefix(dir, runDir) {
		t.Errorf("attempt dir %s is inside the run dir %s", dir, runDir)
	}
	if _, err := os.Lstat(filepath.Join(work, "wt", string(id))); !os.IsNotExist(err) {
		t.Errorf("the shared per-mission worktree was created on the M1 path: %v", err)
	}
	fi, err := os.Stat(wt)
	if err != nil || fi.Mode().Perm() != 0o555 || fi.Mode()&os.ModeSetgid != 0 {
		t.Errorf("worktree mode %v (%v), want 0555 without setgid", fi.Mode(), err)
	}
	var wall string
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*)::text FROM events WHERE mission_id = $1 AND payload::text LIKE '%' || $2 || '%'`, string(id), r.sha).Scan(&wall); err != nil || wall != "0" {
		t.Errorf("an event carries the config sha (%s, %v): AC-22 says the config is not stored in events", wall, err)
	}
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
