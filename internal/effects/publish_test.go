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
	b, _ := json.Marshal(map[string]string{"repo": f.work, "remote": "origin", "branch": "main", "expected_head": expected, "new_head": next})
	return b
}

func pubSpec(f *gitFixture, key, expected, next string) IntentSpec {
	return IntentSpec{Mission: "m_1", Attempt: "r_1", Fence: 1, Kind: "publish", Subject: "branch:origin/main",
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
	ex := &Publish{Root: f.root}
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
	ex := &Publish{Root: f.root}
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
	ex := &Publish{Root: f.root}
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
	ex := &Publish{Root: f.root}
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
	ex := &Publish{Root: f.root}
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
	ex := &Publish{Root: f.root}
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
	branch := func(s *IntentSpec, v string) { s.Request = set("branch", v); s.Subject = "branch:origin/" + v }
	remote := func(s *IntentSpec, v string) { s.Request = set("remote", v); s.Subject = "branch:" + v + "/main" }
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
		{"option remote", func(s *IntentSpec) { remote(s, "--force") }},
		{"url remote", func(s *IntentSpec) { remote(s, "ext::sh -c id") }},
		{"path remote", func(s *IntentSpec) { remote(s, "/tmp/elsewhere.git") }},
		{"force flag", func(s *IntentSpec) { s.Request = set("force", true) }},
		{"missing field", func(s *IntentSpec) { s.Request = with(func(m map[string]any) { delete(m, "expected_head") }) }},
		{"relative repo", func(s *IntentSpec) { s.Request = set("repo", "work") }},
		{"repo outside root", func(s *IntentSpec) { s.Request = set("repo", outside) }},
		{"repo through a symlink out of root", func(s *IntentSpec) { s.Request = set("repo", filepath.Join(f.root, "escape")) }},
		{"unclean repo path", func(s *IntentSpec) { s.Request = set("repo", f.root+"/x/../work") }},
		{"repo is the root", func(s *IntentSpec) { s.Request = set("repo", f.root) }},
		{"subject mismatch", func(s *IntentSpec) { s.Subject = "branch:origin/other" }},
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
}

// Review nit 3 on 11145f4: what could outlive the process group or
// redirect git is off: repository hooks, the ext:: transport, and an
// inherited GIT_* override.
func TestPublishGitHelpersDisabled(t *testing.T) {
	f := newGitFixture(t)
	ex := &Publish{Root: f.root}
	mark := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(f.work, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\ntouch '"+mark+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, f.work, "remote", "add", "evil", "ext::sh -c touch% "+mark)
	t.Setenv("GIT_DIR", f.remote) // would point every command at the bare remote
	ctx := context.Background()
	e := Effect{Kind: "publish", Subject: "branch:origin/main", IdemKey: "h", Request: f.req(f.a, f.b)}
	if rc, err := ex.Dispatch(ctx, Submission{Effect: e, RemoteKey: e.IdemKey}); err != nil || rc.Outcome != OutcomeConfirmed {
		t.Fatalf("push: %+v %v", rc, err)
	}
	if h := f.remoteHead(); h != f.b {
		t.Fatalf("remote head %s, want %s", h, f.b)
	}
	raw, _ := json.Marshal(map[string]string{"repo": f.work, "remote": "evil", "branch": "main", "expected_head": f.a, "new_head": f.b})
	evil := Effect{Kind: "publish", Subject: "branch:evil/main", IdemKey: "x", Request: raw}
	if _, _, err := ex.Precondition(ctx, evil); err == nil {
		t.Error("an ext:: remote was read")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a repository hook or an ext:: command ran")
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
	e := Effect{ID: "e_p", Kind: "publish", Subject: "branch:origin/main", IdemKey: "contract", Request: f.req(f.a, f.b)}
	assertHonoursDeadline(t, "Publish", &Publish{Root: f.root, Git: hung}, e)
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
	ex := &Publish{Root: f.root}
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
