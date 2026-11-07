//go:build unix

package effects

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitFixture is a bare remote with main at a, and a publishing clone (remote
// origin) holding a <- b <- d. other is a second clone that can move the
// remote head to a sibling commit c.
type gitFixture struct {
	t                   *testing.T
	root                string // the executor's allowed root
	remote, work, other string
	a, b, d             string
}

// gitT runs git for a fixture with no user or system configuration.
func gitT(t *testing.T, dir string, args ...string) string {
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

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	f := &gitFixture{t: t, root: root, remote: filepath.Join(root, "remote.git"), work: filepath.Join(root, "work"), other: filepath.Join(root, "other")}
	gitT(t, root, "init", "-q", "--bare", "-b", "main", f.remote)
	gitT(t, root, "init", "-q", "-b", "main", f.work)
	gitT(t, f.work, "commit", "-q", "--allow-empty", "-m", "a")
	f.a = gitT(t, f.work, "rev-parse", "HEAD")
	gitT(t, f.work, "remote", "add", "origin", f.remote)
	gitT(t, f.work, "push", "-q", "origin", "main")
	gitT(t, f.work, "commit", "-q", "--allow-empty", "-m", "b")
	f.b = gitT(t, f.work, "rev-parse", "HEAD")
	gitT(t, f.work, "commit", "-q", "--allow-empty", "-m", "d")
	f.d = gitT(t, f.work, "rev-parse", "HEAD")
	gitT(t, root, "clone", "-q", f.remote, f.other)
	return f
}

// moveHead pushes a sibling commit c (child of a) from the other clone.
func (f *gitFixture) moveHead() string {
	gitT(f.t, f.other, "commit", "-q", "--allow-empty", "-m", "c")
	c := gitT(f.t, f.other, "rev-parse", "HEAD")
	gitT(f.t, f.other, "push", "-q", "origin", "HEAD:main")
	return c
}

func (f *gitFixture) remoteHead() string { return gitT(f.t, f.remote, "rev-parse", "refs/heads/main") }

func (f *gitFixture) req(expected, next string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"repo": f.work, "url": f.remote, "branch": "main", "expected_head": expected, "new_head": next})
	return b
}

// subject is the destination subject of the fixture's main branch.
func (f *gitFixture) subject() string { return "branch:" + f.remote + "#main" }

// exec is the executor under test: the fixture remote is a local path.
func (f *gitFixture) exec() *Publish { return &Publish{Root: f.root, AllowLocal: true} }

// effect is an unstored publish effect for direct executor calls.
func (f *gitFixture) effect(expected, next string) Effect {
	return Effect{Kind: "publish", Subject: f.subject(), IdemKey: "direct", Request: f.req(expected, next)}
}

func pubSpec(f *gitFixture, key, expected, next string) IntentSpec {
	return IntentSpec{Mission: "m_1", Attempt: "r_1", Fence: 1, Kind: "publish", Subject: f.subject(),
		IdemKey: key, Request: f.req(expected, next), NeedsApproval: true}
}

// submitApproved submits with sb and approves with the app-side broker ab.
func submitApproved(t *testing.T, ctx context.Context, sb, ab *Broker, ex Executor, s IntentSpec) Effect {
	t.Helper()
	e, err := sb.Submit(ctx, ex, s)
	if err != nil || e.State != ApprovalRequired {
		t.Fatalf("submit: %+v %v", e, err)
	}
	if err := ab.Approve(ctx, e.ID, e.RequestSHA, "eric"); err != nil {
		t.Fatal(err)
	}
	return e
}

func dispatchRows(t *testing.T, ctx context.Context, b *Broker, id string) int {
	t.Helper()
	var n int
	if err := b.pool.QueryRow(ctx, "SELECT count(*) FROM effect_dispatches WHERE effect_id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPublishHappyPath(t *testing.T) {
	f := newGitFixture(t)
	ctx, _, b, _ := setup(t)
	ex := f.exec()
	e := submitApproved(t, ctx, b, b, ex, pubSpec(f, "p_ok", f.a, f.b))
	got, err := b.Dispatch(ctx, ex, e.ID)
	if err != nil || got.State != Confirmed || got.Dispatches != 1 {
		t.Fatalf("dispatch: %+v %v", got, err)
	}
	if h := f.remoteHead(); h != f.b {
		t.Fatalf("remote head %s, want %s", h, f.b)
	}
	if _, err := b.Dispatch(ctx, ex, e.ID); err == nil {
		t.Fatal("a second dispatch of a CONFIRMED publish was accepted")
	}
	if obs, _, err := ex.Reconcile(ctx, got); err != nil || obs != ObsCommitted {
		t.Fatalf("readback after the push: %s %v", obs, err)
	}
}

// AC-12: the branch head moves between INTENT and DISPATCH. The effect is
// QUARANTINED, never pushed, and the branch keeps the other writer's head.
func TestPublishMovedHeadQuarantined(t *testing.T) {
	f := newGitFixture(t)
	ctx, _, b, _ := setup(t)
	ex := f.exec()
	e := submitApproved(t, ctx, b, b, ex, pubSpec(f, "p_moved", f.a, f.b))
	c := f.moveHead()
	got, err := b.Dispatch(ctx, ex, e.ID)
	if err != nil || got.State != Quarantined || got.Dispatches != 0 || !strings.Contains(got.LastError, "precondition") {
		t.Fatalf("dispatch after the head moved: %+v %v, want QUARANTINED with no dispatch", got, err)
	}
	if h := f.remoteHead(); h != c {
		t.Fatalf("remote head %s, want the other writer's %s (branch touched)", h, c)
	}
	if n := dispatchRows(t, ctx, b, e.ID); n != 0 {
		t.Fatalf("dispatch rows %d, want 0", n)
	}
	// Review 4: it was never sent, so it cannot resolve as applied, by the
	// broker or by a raw write with its audit row.
	if _, err := b.ResolveQuarantined(ctx, e.ID, ResolvedApplied, "says so", "wal:w_test", "eric"); err == nil {
		t.Fatal("a never-dispatched publish resolved as applied")
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ($1, 'QUARANTINED', 'RESOLVED_APPLIED', 'r', 'e', 'eric')`, e.ID) //nolint:errcheck
	if _, err := tx.Exec(ctx, "UPDATE effects SET state = 'RESOLVED_APPLIED' WHERE id = $1", e.ID); err == nil {
		t.Error("the guard let a never-dispatched effect resolve as applied")
	}
	tx.Rollback(ctx) //nolint:errcheck
	// The operator exit still applies.
	if got, err := b.ResolveQuarantined(ctx, e.ID, ResolvedNotApplied, "head moved before publish", "wal:w_test", "eric"); err != nil || got.State != ResolvedNotApplied {
		t.Fatalf("resolve: %+v %v", got, err)
	}
}

// AC-12, the race: the head moves after the precondition read it. The lease
// refuses the push, the outcome is UNKNOWN, and readback quarantines it.
func TestPublishHeadMovesDuringDispatch(t *testing.T) {
	f := newGitFixture(t)
	ctx, _, b, _ := setup(t)
	ex := f.exec()
	e := submitApproved(t, ctx, b, b, ex, pubSpec(f, "p_race", f.a, f.b))
	var c string
	b.afterDispatchCommit = func(Effect) { c = f.moveHead() }
	got, err := b.Dispatch(ctx, ex, e.ID)
	b.afterDispatchCommit = nil
	if err != nil || got.State != Unknown {
		t.Fatalf("dispatch with the head moved under it: %+v %v, want UNKNOWN", got, err)
	}
	if h := f.remoteHead(); h != c {
		t.Fatalf("remote head %s, want %s (branch touched)", h, c)
	}
	got, err = b.Reconcile(ctx, ex, e.ID)
	if err != nil || got.State != Quarantined {
		t.Fatalf("readback: %+v %v, want QUARANTINED", got, err)
	}
	if h := f.remoteHead(); h != c {
		t.Fatalf("remote head %s after readback, want %s", h, c)
	}
}

// NEG-12: a stale expected_head refuses. The remote already holds b (pushed
// by someone else), the effect row says a, and d descends from b, so a plain
// push would fast-forward; only the expected-head check stops it.
func TestPublishStaleExpectedHeadRefused(t *testing.T) {
	f := newGitFixture(t)
	ctx, _, b, _ := setup(t)
	ex := f.exec()
	gitT(t, f.work, "push", "-q", "origin", f.b+":refs/heads/main")
	e := submitApproved(t, ctx, b, b, ex, pubSpec(f, "p_stale", f.a, f.d))
	var rowExpected string
	if err := b.pool.QueryRow(ctx, "SELECT request->>'expected_head' FROM effects WHERE id = $1", e.ID).Scan(&rowExpected); err != nil || rowExpected != f.a {
		t.Fatalf("effect row expected_head %q %v, want %s", rowExpected, err, f.a)
	}
	got, err := b.Dispatch(ctx, ex, e.ID)
	if err != nil || got.State != Quarantined || got.Dispatches != 0 {
		t.Fatalf("dispatch with a stale expected head: %+v %v, want QUARANTINED", got, err)
	}
	if h := f.remoteHead(); h != f.b {
		t.Fatalf("remote head %s, want %s", h, f.b)
	}
	// The transport itself refuses too: the lease binds expected_head even
	// when the precondition is bypassed.
	rc, err := ex.Dispatch(ctx, Submission{Effect: e, RemoteKey: e.IdemKey})
	if err == nil && rc.Outcome == OutcomeConfirmed {
		t.Fatalf("a push with a stale lease was confirmed: %+v", rc)
	}
	if h := f.remoteHead(); h != f.b {
		t.Fatalf("remote head %s after a stale-lease push, want %s", h, f.b)
	}
	if obs, _, err := ex.Reconcile(ctx, e); err != nil || obs != ObsConflict {
		t.Fatalf("readback of a stale publish: %s %v, want CONFLICT", obs, err)
	}
}

// Never a force push: a new head that does not descend from expected_head
// fails definitely, and the branch is untouched.
func TestPublishRefusesNonFastForward(t *testing.T) {
	f := newGitFixture(t)
	ctx, _, b, _ := setup(t)
	ex := f.exec()
	orphan := gitT(t, f.work, "commit-tree", f.a+"^{tree}", "-m", "orphan")
	e := submitApproved(t, ctx, b, b, ex, pubSpec(f, "p_nonff", f.a, orphan))
	got, err := b.Dispatch(ctx, ex, e.ID)
	if err != nil || got.State != FailedDefinite || got.Dispatches != 0 {
		t.Fatalf("dispatch of a non-fast-forward: %+v %v, want FAILED_DEFINITE", got, err)
	}
	if h := f.remoteHead(); h != f.a {
		t.Fatalf("remote head %s, want %s", h, f.a)
	}
	// The lease alone would allow this rewrite; Dispatch rechecks ancestry.
	rc, err := ex.Dispatch(ctx, Submission{Effect: got, RemoteKey: got.IdemKey})
	if err == nil && rc.Outcome == OutcomeConfirmed {
		t.Fatalf("a non-fast-forward transport was confirmed: %+v", rc)
	}
	if h := f.remoteHead(); h != f.a {
		t.Fatalf("remote head %s after a direct non-fast-forward push, want %s", h, f.a)
	}
}

func TestPublishValidate(t *testing.T) {
	f := newGitFixture(t)
	ex := f.exec()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.root, "escape")); err != nil {
		t.Fatal(err)
	}
	ok := pubSpec(f, "v", f.a, f.b)
	if err := ex.Validate(ok); err != nil {
		t.Fatalf("a valid publish was refused: %v", err)
	}
	with := func(mut func(map[string]any)) json.RawMessage {
		m := map[string]any{}
		json.Unmarshal(ok.Request, &m) //nolint:errcheck
		mut(m)
		b, _ := json.Marshal(m)
		return b
	}
	set := func(k string, v any) json.RawMessage { return with(func(m map[string]any) { m[k] = v }) }
	branch := func(s *IntentSpec, v string) {
		s.Request = set("branch", v)
		s.Subject = "branch:" + f.remote + "#" + v
	}
	remote := func(s *IntentSpec, v string) { s.Request = set("url", v); s.Subject = "branch:" + v + "#main" }
	for _, u := range []string{"https://github.com/o/r.git", "ssh://git@github.com/o/r.git", "git@github.com:o/r.git"} {
		s := ok
		remote(&s, u)
		if err := ex.Validate(s); err != nil {
			t.Errorf("url %s refused: %v", u, err)
		}
	}
	cases := []struct {
		name string
		mut  func(*IntentSpec)
	}{
		{"no approval", func(s *IntentSpec) { s.NeedsApproval = false }},
		{"short sha", func(s *IntentSpec) { s.Request = set("expected_head", f.a[:12]) }},
		{"uppercase sha", func(s *IntentSpec) { s.Request = set("new_head", strings.ToUpper(f.b)) }},
		{"symbolic head", func(s *IntentSpec) { s.Request = set("new_head", "HEAD") }},
		{"same heads", func(s *IntentSpec) { s.Request = set("new_head", f.a) }},
		{"force refspec", func(s *IntentSpec) { branch(s, "+main") }},
		{"colon refspec", func(s *IntentSpec) { branch(s, "main:other") }},
		{"option branch", func(s *IntentSpec) { branch(s, "-f") }},
		{"dotdot branch", func(s *IntentSpec) { branch(s, "a..b") }},
		{"lock branch", func(s *IntentSpec) { branch(s, "main.lock") }},
		{"hidden component", func(s *IntentSpec) { branch(s, "a/.b") }},
		{"empty component", func(s *IntentSpec) { branch(s, "a//b") }},
		{"option url", func(s *IntentSpec) { remote(s, "--upload-pack=id") }},
		{"ext url", func(s *IntentSpec) { remote(s, "ext::sh -c id") }},
		{"fd url", func(s *IntentSpec) { remote(s, "fd::3") }},
		{"remote name", func(s *IntentSpec) { remote(s, "origin") }},
		{"file url", func(s *IntentSpec) { remote(s, "file:///tmp/x.git") }},
		{"http url", func(s *IntentSpec) { remote(s, "http://github.com/o/r.git") }},
		{"https with a token", func(s *IntentSpec) { remote(s, "https://x:tok@github.com/o/r.git") }},
		{"https with a user", func(s *IntentSpec) { remote(s, "https://x@github.com/o/r.git") }},
		{"ssh with a password", func(s *IntentSpec) { remote(s, "ssh://git:pw@github.com/o/r.git") }},
		{"option host", func(s *IntentSpec) { remote(s, "ssh://-oProxyCommand=id/r.git") }},
		{"scp option host", func(s *IntentSpec) { remote(s, "git@-oProxyCommand=id:r.git") }},
		{"url with a query", func(s *IntentSpec) { remote(s, "https://github.com/o/r.git?x=1") }},
		{"unclean path url", func(s *IntentSpec) { remote(s, f.root+"/x/../remote.git") }},
		{"force flag", func(s *IntentSpec) { s.Request = set("force", true) }},
		{"missing field", func(s *IntentSpec) { s.Request = with(func(m map[string]any) { delete(m, "expected_head") }) }},
		{"relative repo", func(s *IntentSpec) { s.Request = set("repo", "work") }},
		{"repo outside root", func(s *IntentSpec) { s.Request = set("repo", outside) }},
		{"repo through a symlink out of root", func(s *IntentSpec) { s.Request = set("repo", filepath.Join(f.root, "escape")) }},
		{"unclean repo path", func(s *IntentSpec) { s.Request = set("repo", f.root+"/x/../work") }},
		{"repo is the root", func(s *IntentSpec) { s.Request = set("repo", f.root) }},
		{"subject mismatch", func(s *IntentSpec) { s.Subject = "branch:" + f.remote + "#other" }},
	}
	for _, c := range cases {
		s := ok
		c.mut(&s)
		if err := ex.Validate(s); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if err := (&Publish{}).Validate(ok); err == nil {
		t.Error("an executor with no root accepted a publish")
	}
	if err := (&Publish{Root: f.root}).Validate(ok); err == nil {
		t.Error("a local path url was accepted without AllowLocal")
	}
}

// Review 1 on 4d98e31: the approved URL is where the push goes. Repository
// redirection (pushInsteadOf, insteadOf) is refused before any transport;
// a remote's pushurl and the user's global configuration do not apply.
func TestPublishBindsDestination(t *testing.T) {
	ctx := context.Background()
	newDecoy := func(f *gitFixture) string {
		d := filepath.Join(f.root, "decoy.git")
		gitT(t, f.root, "clone", "-q", "--bare", f.remote, d)
		return d
	}
	for _, key := range []string{"pushInsteadOf", "insteadOf"} {
		f := newGitFixture(t)
		decoy := newDecoy(f)
		gitT(t, f.work, "config", "url."+decoy+"."+key, f.remote)
		ex := f.exec()
		e := f.effect(f.a, f.b)
		if _, _, err := ex.Precondition(ctx, e); err == nil {
			t.Errorf("%s: precondition accepted a redirecting repository", key)
		}
		if rc, err := ex.Dispatch(ctx, Submission{Effect: e, RemoteKey: "k"}); err == nil {
			t.Errorf("%s: dispatch went ahead: %+v", key, rc)
		}
		if obs, _, err := ex.Reconcile(ctx, e); err == nil || obs != ObsUnknown {
			t.Errorf("%s: readback from a redirecting repository: %s %v", key, obs, err)
		}
		if d := gitT(t, decoy, "rev-parse", "refs/heads/main"); d != f.a {
			t.Errorf("%s: decoy moved to %s", key, d)
		}
		if h := f.remoteHead(); h != f.a {
			t.Errorf("%s: remote moved to %s", key, h)
		}
	}
	// A pushurl on a named remote and a global pushInsteadOf are ignored:
	// the push goes to the approved URL.
	f := newGitFixture(t)
	decoy := newDecoy(f)
	gitT(t, f.work, "config", "remote.origin.pushurl", decoy)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[url \""+decoy+"\"]\n\tpushInsteadOf = "+f.remote+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	ex := f.exec()
	if rc, err := ex.Dispatch(ctx, Submission{Effect: f.effect(f.a, f.b), RemoteKey: "k"}); err != nil || rc.Outcome != OutcomeConfirmed {
		t.Fatalf("push: %+v %v", rc, err)
	}
	if d := gitT(t, decoy, "rev-parse", "refs/heads/main"); d != f.a {
		t.Errorf("decoy moved to %s", d)
	}
	if h := f.remoteHead(); h != f.b {
		t.Errorf("remote head %s, want %s", h, f.b)
	}
}

// Review 2 on 4d98e31: the git dir and object store are bound to Root too.
func TestPublishBindsGitDir(t *testing.T) {
	ctx := context.Background()
	f := newGitFixture(t)
	outside := t.TempDir()
	gitT(t, outside, "clone", "-q", f.work, "real")
	linked := filepath.Join(f.root, "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+filepath.Join(outside, "real", ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := f.exec()
	at := func(repo string) Effect {
		e := f.effect(f.a, f.b)
		e.Request = []byte(strings.Replace(string(e.Request), `"repo":"`+f.work+`"`, `"repo":"`+repo+`"`, 1))
		if !strings.Contains(string(e.Request), repo) {
			t.Fatalf("request not rewritten: %s", e.Request)
		}
		return e
	}
	if _, _, err := ex.Precondition(ctx, at(linked)); err == nil || !strings.Contains(err.Error(), "git dir") {
		t.Errorf("a git dir outside Root was used: %v", err)
	}
	// Borrowed objects: alternates into a directory outside Root.
	alt := filepath.Join(f.root, "alt")
	gitT(t, f.root, "clone", "-q", "--shared", f.work, alt)
	if _, _, err := ex.Precondition(ctx, at(alt)); err == nil || !strings.Contains(err.Error(), "borrows") {
		t.Errorf("a repository with alternates was used: %v", err)
	}
	// A worktree redirection in config.
	gitT(t, f.work, "config", "core.worktree", outside)
	if _, _, err := ex.Precondition(ctx, f.effect(f.a, f.b)); err == nil {
		t.Error("a repository with core.worktree was used")
	}
	if h := f.remoteHead(); h != f.a {
		t.Fatalf("remote moved to %s", h)
	}
}

// Review 3 on 4d98e31: git sees only an allow-listed environment.
func TestPublishEnvAllowListed(t *testing.T) {
	f := newGitFixture(t)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nenv > '"+dump+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGPASSWORD", "not-a-secret")
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", dir)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url.x.insteadof'='y'")
	ex := &Publish{Root: f.root, Git: fake, AllowLocal: true}
	ex.Precondition(context.Background(), f.effect(f.a, f.b)) //nolint:errcheck
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake git did not run: %v", err)
	}
	allowed := map[string]bool{"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true, "SSH_AUTH_SOCK": true,
		"HOME": true, "GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true, "GIT_SSH_COMMAND": true,
		"GIT_CEILING_DIRECTORIES": true, "PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, _, _ := strings.Cut(line, "=")
		n++
		if !allowed[k] {
			t.Errorf("git saw %s", k)
		}
	}
	if n < 5 || !strings.Contains(string(raw), "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Fatalf("env dump looks empty (%d lines)", n)
	}
}

// Review nit 3 on 11145f4: what could outlive the process group or
// redirect git is off: repository hooks and an inherited GIT_* override.
func TestPublishGitHelpersDisabled(t *testing.T) {
	f := newGitFixture(t)
	ex := f.exec()
	mark := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(f.work, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\ntouch '"+mark+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", f.remote) // would point every command at the bare remote
	ctx := context.Background()
	if rc, err := ex.Dispatch(ctx, Submission{Effect: f.effect(f.a, f.b), RemoteKey: "h"}); err != nil || rc.Outcome != OutcomeConfirmed {
		t.Fatalf("push: %+v %v", rc, err)
	}
	if h := f.remoteHead(); h != f.b {
		t.Fatalf("remote head %s, want %s", h, f.b)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a repository hook ran")
	}
}

// The deadline contract with a git that never answers: every call returns
// by its deadline, and the fake git was really reached each time.
func TestPublishHonoursDeadline(t *testing.T) {
	f := newGitFixture(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "calls")
	hung := filepath.Join(dir, "git")
	if err := os.WriteFile(hung, []byte("#!/bin/sh\necho call >> '"+mark+"'\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := f.effect(f.a, f.b)
	assertHonoursDeadline(t, "Publish", &Publish{Root: f.root, Git: hung, AllowLocal: true}, e)
	raw, _ := os.ReadFile(mark)
	if n := strings.Count(string(raw), "call"); n != 3 {
		t.Fatalf("hung git reached %d times, want 3 (the contract test would be vacuous)", n)
	}
}

// Migration 0024: only a publish may move APPROVED (or INTENT) to
// QUARANTINED, without a dispatch or receipt; a publish is born needing
// approval; the runner role can quarantine its own live dispatch.
func TestPublishQuarantineGuard(t *testing.T) {
	f := newGitFixture(t)
	ctx, st, b, _ := setup(t)
	ackRun(t, ctx, st)
	_, rb := runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	ins := `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ($1, 'm_1', 'r_1', 1, 'publish', $1, $1, '{}', 'sha', $2)`
	if _, err := ab.pool.Exec(ctx, ins, "e_pub_intent", "INTENT"); err == nil {
		t.Error("a publish was born INTENT, skipping its approval")
	}
	if _, err := ab.pool.Exec(ctx, ins, "e_pub_ar", "APPROVAL_REQUIRED"); err != nil {
		t.Fatalf("a publish born APPROVAL_REQUIRED was refused: %v", err)
	}
	if _, err := ab.pool.Exec(ctx, "UPDATE effects SET state = 'QUARANTINED' WHERE id = 'e_pub_ar'"); err == nil {
		t.Error("APPROVAL_REQUIRED moved to QUARANTINED")
	}
	ex := f.exec()
	e := submitApproved(t, ctx, rb, ab, ex, pubSpec(f, "p_guard", f.a, f.b))
	for _, q := range []string{
		"UPDATE effects SET state = 'QUARANTINED', dispatches = dispatches + 1 WHERE id = $1",
		`UPDATE effects SET state = 'QUARANTINED', receipt = '{"outcome":"CONFLICT"}' WHERE id = $1`,
	} {
		if _, err := ab.pool.Exec(ctx, q, e.ID); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	c := f.moveHead()
	got, err := rb.Dispatch(ctx, ex, e.ID)
	if err != nil || got.State != Quarantined {
		t.Fatalf("runner dispatch after the head moved: %+v %v, want QUARANTINED", got, err)
	}
	if h := f.remoteHead(); h != c {
		t.Fatalf("remote head %s, want %s", h, c)
	}
	if _, err := b.pool.Exec(ctx, "UPDATE effects SET state = 'APPROVED' WHERE id = $1", e.ID); err == nil {
		t.Error("QUARANTINED re-armed to APPROVED")
	}
}
