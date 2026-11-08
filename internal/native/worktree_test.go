package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
	"fabric/internal/tools"
	"fabric/internal/tools/fakehost"
)

// goodqa#15283: the attempt-to-worktree binding.

func textStep(s string) fakeapi.Step { return fakeapi.Step{Events: fakeapi.Text(s, 50, 5)} }

// storedWorktree reads attempts.worktree ("" for NULL).
func storedWorktree(t *testing.T, st *store.Store, a string) (string, bool) {
	t.Helper()
	var wt *string
	if err := st.Pool().QueryRow(context.Background(), `SELECT worktree FROM attempts WHERE id = $1`, a).Scan(&wt); err != nil {
		t.Fatal(err)
	}
	if wt == nil {
		return "", false
	}
	return *wt, true
}

// seedStartedBound starts and acks a native attempt through the ordinary
// commands, bound to the canonical form of worktree, with no agent_runs row:
// a crash before the loop's first checkpoint, which Run resumes.
func seedStartedBound(t *testing.T, st *store.Store, e *env, worktree string) int64 {
	t.Helper()
	ctx := context.Background()
	m, a := domain.MissionID(e.g.Mission), domain.AttemptID(e.g.Attempt)
	if err := st.CreateMission(ctx, m, domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(m)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	lock, err := native.LockPath(a)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := st.Execute(ctx, "native-runner", "seed-start", domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a,
		Host: hostid.Default(), Role: "writer", Path: domain.ProviderAnthropicAPI, LockPath: lock, Worktree: canon(t, worktree)})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "native-runner", "seed-ack", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	return fence
}

// untouched is what a refused run must leave as it was.
type untouched struct {
	commands, events int
	lease            string
}

func snapshot(t *testing.T, st *store.Store, a string) untouched {
	t.Helper()
	var u untouched
	u.commands = count(t, st, "SELECT count(*) FROM commands")
	u.events = count(t, st, "SELECT count(*) FROM events")
	if err := st.Pool().QueryRow(context.Background(), `SELECT coalesce(lease_until::text, 'NULL') FROM attempts WHERE id = $1`, a).Scan(&u.lease); err != nil {
		t.Fatal(err)
	}
	return u
}

// assertRefusedClean checks the refusal left no trace: no key command, no
// SelfTest or hook, no request, no lock file, no command, event or lease change.
func assertRefusedClean(t *testing.T, st *store.Store, e *env, hookMarker string, before untouched) {
	t.Helper()
	if exists(e.marker) {
		t.Error("the key command ran")
	}
	if exists(hookMarker) {
		t.Error("SelfTest or an oracle hook ran")
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
	lock, _ := native.LockPath(domain.AttemptID(e.g.Attempt))
	if exists(lock) {
		t.Error("the lock file was taken")
	}
	if after := snapshot(t, st, e.g.Attempt); after != before {
		t.Errorf("state changed: %+v -> %+v", before, after)
	}
}

// AC-1: a new native attempt stores the canonical worktree; a grant given
// through a symlink stores the resolved target.
func TestNativeStoresCanonicalWorktree(t *testing.T) {
	stateDir(t)
	st := storetest.Open(t)
	e := newEnv(t, textStep("finished"))
	real := canon(t, e.g.Worktree)
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e.g.Worktree = link
	if _, err := native.Run(context.Background(), st, e.g, opts(true)); err != nil {
		t.Fatal(err)
	}
	got, ok := storedWorktree(t, st, e.g.Attempt)
	if !ok || got != real {
		t.Fatalf("attempts.worktree = %q (set %v), want the resolved %q, not the grant's %q", got, ok, real, link)
	}
	var ev string
	if err := st.Pool().QueryRow(context.Background(), `SELECT payload->>'worktree' FROM events WHERE kind = 'ATTEMPT_STARTED' AND mission_id = $1`, e.g.Mission).Scan(&ev); err != nil || ev != real {
		t.Fatalf("ATTEMPT_STARTED worktree %q (%v), want %q", ev, err, real)
	}
}

// AC-2: a resume whose canonical grant worktree equals the stored one
// proceeds as before, also through another symlink spelling.
func TestNativeResumesSameWorktreeViaSymlink(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	st := storetest.Open(t)
	e := newEnv(t, textStep("resumed"))
	real := canon(t, e.g.Worktree)
	fence := seedStartedBound(t, st, e, real)
	link := filepath.Join(t.TempDir(), "other-spelling")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e.g.Worktree = link
	starts := count(t, st, "SELECT count(*) FROM commands WHERE kind IN ('ADMIT_MISSION', 'START_ATTEMPT')")
	r, err := native.Run(ctx, st, e.g, opts(true))
	if err != nil || r.Outcome != "done" || r.Fence != fence {
		t.Fatalf("resume via a symlink: %+v %v, want done on fence %d", r, err, fence)
	}
	if n := count(t, st, "SELECT count(*) FROM commands WHERE kind IN ('ADMIT_MISSION', 'START_ATTEMPT')"); n != starts {
		t.Fatalf("admit and start commands %d -> %d on resume, want unchanged (caseResume)", starts, n)
	}
	lock, _ := native.LockPath(domain.AttemptID(e.g.Attempt))
	var stored string
	if err := st.Pool().QueryRow(ctx, `SELECT lock_path FROM attempts WHERE id = $1`, e.g.Attempt).Scan(&stored); err != nil || stored != lock || !exists(stored) {
		t.Fatalf("lock path %q (%v): want the stored %q, taken", stored, err, lock)
	}
}

// otherWorktree makes a grant worktree that resolves somewhere else than
// real, in each of the NEG-1 and NEG-2 shapes.
func otherWorktree(t *testing.T, real, how string) (grant, resolved string) {
	t.Helper()
	sib := filepath.Join(filepath.Dir(real), "sibling-wt")
	if err := os.MkdirAll(sib, 0o755); err != nil {
		t.Fatal(err)
	}
	switch how {
	case "sibling directory":
		return sib, sib
	case "symlink to another directory":
		link := filepath.Join(t.TempDir(), "looks-right")
		if err := os.Symlink(sib, link); err != nil {
			t.Fatal(err)
		}
		return link, sib
	case "dot-dot spelling resolving elsewhere":
		return real + "/../sibling-wt", sib
	}
	t.Fatalf("unknown shape %s", how)
	return "", ""
}

var otherShapes = []string{"sibling directory", "symlink to another directory", "dot-dot spelling resolving elsewhere"}

func assertMismatch(t *testing.T, err error, stored, resolved string) {
	t.Helper()
	if !errors.Is(err, native.ErrWorktreeMismatch) || !errors.Is(err, native.ErrStartRefused) {
		t.Fatalf("err %v, want ErrWorktreeMismatch (and ErrStartRefused)", err)
	}
	for _, p := range []string{stored, resolved} {
		if !strings.Contains(err.Error(), strconv.Quote(p)) {
			t.Errorf("refusal %q does not name %q", err, p)
		}
	}
}

// AC-3, NEG-1, NEG-2: a resume from a different worktree is refused with
// ErrWorktreeMismatch before SelfTest, hooks, the key, the flock or a write.
func TestNativeRefusesResumeFromOtherWorktree(t *testing.T) {
	for _, how := range otherShapes {
		t.Run(how, func(t *testing.T) {
			stateDir(t)
			st := storetest.Open(t)
			e := newEnv(t, textStep("never"))
			real := canon(t, e.g.Worktree)
			seedStartedBound(t, st, e, real)
			hook := selfTestMarker(t, e)
			grant, resolved := otherWorktree(t, real, how)
			e.g.Worktree = grant
			before := snapshot(t, st, e.g.Attempt)
			_, err := native.Run(context.Background(), st, e.g, opts(true))
			assertMismatch(t, err, real, resolved)
			assertRefusedClean(t, st, e, hook, before)
		})
	}
}

// AC-3: a replay (the attempt has a recorded outcome) from a different
// worktree is refused the same way; from the bound worktree it replays.
func TestNativeRefusesReplayFromOtherWorktree(t *testing.T) {
	for _, how := range append(append([]string{}, otherShapes...), "control: bound worktree") {
		t.Run(how, func(t *testing.T) {
			stateDir(t)
			st := storetest.Open(t)
			e := newEnv(t, textStep("never"))
			real := canon(t, e.g.Worktree)
			seedRunning(t, st, e, "max_turns", 0) // a recorded outcome: caseReplay
			hook := selfTestMarker(t, e)
			if how == "control: bound worktree" {
				r, err := native.Run(context.Background(), st, e.g, opts(true))
				if err != nil || r.Outcome != "max_turns" || len(e.srv.Requests()) != 0 {
					t.Fatalf("control replay: %+v %v, %d requests; want the recorded outcome, none sent", r, err, len(e.srv.Requests()))
				}
				return
			}
			grant, resolved := otherWorktree(t, real, how)
			e.g.Worktree = grant
			before := snapshot(t, st, e.g.Attempt)
			_, err := native.Run(context.Background(), st, e.g, opts(true))
			assertMismatch(t, err, real, resolved)
			assertRefusedClean(t, st, e, hook, before)
		})
	}
}

// NEG-3: an attempt row with a NULL worktree (written before 0038) is
// refused with ErrWorktreeUnbound on resume and on replay, never adopted.
func TestNativeRefusesUnboundAttempt(t *testing.T) {
	for _, c := range []string{"resume", "replay"} {
		t.Run(c, func(t *testing.T) {
			ctx := context.Background()
			stateDir(t)
			st := storetest.Open(t)
			e := newEnv(t, textStep("never"))
			if err := st.CreateMission(ctx, domain.MissionID(e.g.Mission), domain.ProviderAnthropicAPI, e.g.Ticket, "", ""); err != nil {
				t.Fatal(err)
			}
			lock, err := native.LockPath(domain.AttemptID(e.g.Attempt))
			if err != nil {
				t.Fatal(err)
			}
			// The row a pre-0038 binary writes: no worktree column at all.
			if _, err := st.Pool().Exec(ctx, `INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role, path, lock_path, lease_until)
				VALUES ($1, $2, 1, 0, 0, 'RUNNING', $3, 'writer', 'anthropic-api', $4, now() + interval '1 hour')`,
				e.g.Attempt, e.g.Mission, string(hostid.Default()), lock); err != nil {
				t.Fatal(err)
			}
			if c == "replay" {
				if _, err := st.Pool().Exec(ctx, `INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode, outcome, outcome_note)
					VALUES ($1, 1, 'seed', 'api_key', 'max_turns', 'seeded')`, e.g.Attempt); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := storedWorktree(t, st, e.g.Attempt); ok {
				t.Fatal("setup: the attempt has a worktree")
			}
			hook := selfTestMarker(t, e)
			before := snapshot(t, st, e.g.Attempt)
			_, err = native.Run(ctx, st, e.g, opts(true))
			if !errors.Is(err, native.ErrWorktreeUnbound) || !errors.Is(err, native.ErrStartRefused) || !strings.Contains(err.Error(), "reconcile") {
				t.Fatalf("%s of an unbound attempt: %v, want ErrWorktreeUnbound naming the fix", c, err)
			}
			assertRefusedClean(t, st, e, hook, before)
			if _, ok := storedWorktree(t, st, e.g.Attempt); ok {
				t.Fatal("the refused run adopted a worktree")
			}
		})
	}
}

// NEG-4: a bad grant worktree is refused before any Execute, so the keyed
// start command is never stored and a corrected retry with the same attempt
// id succeeds.
func TestNativeRejectsBadWorktreeBeforeExecute(t *testing.T) {
	for _, c := range []string{"missing", "a file", "root", "not valid UTF-8", "dangling symlink", "symlink loop"} {
		t.Run(c, func(t *testing.T) {
			ctx := context.Background()
			stateDir(t)
			st := storetest.Open(t)
			e := newEnv(t, textStep("finished"))
			good := e.g.Worktree
			dir := t.TempDir()
			bad := filepath.Join(dir, "missing")
			switch c {
			case "a file":
				bad = filepath.Join(dir, "file")
				if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "root":
				bad = "/"
			case "not valid UTF-8":
				bad = filepath.Join(dir, "wt-\xff")
				if err := os.Mkdir(bad, 0o755); err != nil {
					t.Skipf("filesystem refuses a non-UTF-8 name: %v", err)
				}
			case "dangling symlink":
				bad = filepath.Join(dir, "dangling")
				if err := os.Symlink(filepath.Join(dir, "nowhere"), bad); err != nil {
					t.Fatal(err)
				}
			case "symlink loop":
				bad = filepath.Join(dir, "loop-a")
				if err := os.Symlink(filepath.Join(dir, "loop-b"), bad); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(bad, filepath.Join(dir, "loop-b")); err != nil {
					t.Fatal(err)
				}
			}
			e.g.Worktree = bad
			_, err := native.Run(ctx, st, e.g, opts(true))
			if !errors.Is(err, native.ErrStartRefused) {
				t.Fatalf("worktree %q: %v, want ErrStartRefused", bad, err)
			}
			if n := count(t, st, "SELECT count(*) FROM commands"); n != 0 {
				t.Fatalf("%d commands stored by a refused start", n)
			}
			if n := count(t, st, "SELECT count(*) FROM attempts"); n != 0 {
				t.Fatalf("%d attempts after a refused start", n)
			}
			if exists(e.marker) || len(e.srv.Requests()) != 0 {
				t.Fatal("the key was read or a request was sent")
			}
			e.g.Worktree = good
			r, err := native.Run(ctx, st, e.g, opts(true))
			if err != nil || r.Outcome != "done" {
				t.Fatalf("corrected retry with the same attempt id: %+v %v, want done", r, err)
			}
			if got, _ := storedWorktree(t, st, e.g.Attempt); got != canon(t, good) {
				t.Fatalf("retry stored %q, want %q", got, canon(t, good))
			}
		})
	}
}

// movedFixture puts the worktree at <dir>/parent/wt and grants it through a
// symlink, so the canonical path differs from the grant's spelling.
func movedFixture(t *testing.T, e *env) (canonical string, move func()) {
	t.Helper()
	dir := canon(t, t.TempDir())
	parent := filepath.Join(dir, "parent")
	canonical = filepath.Join(parent, "wt")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "notes.txt"), []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "grant-link")
	if err := os.Symlink(canonical, link); err != nil {
		t.Fatal(err)
	}
	e.g.Worktree = link
	return canonical, func() {
		// The parent is renamed away and the same path recreated as a new
		// directory: the path still resolves to itself; only (dev, ino) differ.
		if err := os.Rename(parent, parent+".moved"); err != nil {
			t.Error(err)
			return
		}
		if err := os.MkdirAll(canonical, 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(canonical, "notes.txt"), []byte("alpha PLANTED\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
}

// AC-5: after a parent-directory swap mid-run, the next start of read, grep,
// shell and an oracle hook is refused with ErrWorktreeMoved; before it, the
// tool cwd and the read root are the canonical path.
func TestNativeToolRefusesMovedWorktree(t *testing.T) {
	moved := native.ErrWorktreeMoved.Error()
	t.Run("in a run", func(t *testing.T) {
		stateDir(t)
		st := storetest.Open(t)
		e := newEnv(t,
			fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_pwd", "shell", `{"command":"pwd"}`, 50, 5)},
			fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_read", "read", `{"path":"notes.txt"}`, 50, 5)},
			fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_grep", "grep", `{"pattern":"alpha"}`, 50, 5)},
			fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_echo", "shell", `{"command":"echo hi"}`, 50, 5)},
			textStep("finished"),
		)
		e.g.ShellAllow = append(e.g.ShellAllow, `^pwd$`)
		canonical, move := movedFixture(t, e)
		hook := selfTestMarker(t, e)
		var mu sync.Mutex
		n := 0
		e.srv.Validate = func([]byte) error {
			mu.Lock()
			defer mu.Unlock()
			if n++; n == 2 { // the request carrying the pwd result
				move()
				os.Remove(hook) // SelfTest touched it; no hook may run from here on
			}
			return nil
		}
		o := opts(true)
		fh := fakehost.New()
		o.Starter = fh
		r, err := native.Run(context.Background(), st, e.g, o)
		if err != nil || r.Outcome != "done" {
			t.Fatalf("run: %+v %v", r, err)
		}
		reqs := e.srv.Requests()
		if len(reqs) != 5 {
			t.Fatalf("%d requests, want 5", len(reqs))
		}
		if !strings.Contains(string(reqs[1].Body), canonical) || strings.Contains(string(reqs[1].Body), e.g.Worktree) {
			t.Fatalf("pwd result does not show the canonical cwd %q: %s", canonical, reqs[1].Body)
		}
		for i, what := range []string{"read", "grep", "shell (oracle hook)"} {
			body := string(reqs[2+i].Body)
			if got := strings.Count(body, moved); got < i+1 {
				t.Errorf("%s after the move: %d refusals naming %q in the next request, want %d", what, got, moved, i+1)
			}
		}
		for _, q := range reqs {
			if strings.Contains(string(q.Body), "PLANTED") {
				t.Fatal("a tool read the directory planted at the recorded path")
			}
		}
		if len(fh.Starts) != 1 {
			t.Fatalf("%d host starts, want only the pwd before the move", len(fh.Starts))
		}
		if exists(hook) {
			t.Fatal("an oracle hook ran after the move")
		}
	})

	t.Run("each entry point as Run builds it", func(t *testing.T) {
		e := newEnv(t)
		e.g.ShellAllow = []string{`^echo hi$`}
		canonical, move := movedFixture(t, e)
		hook := selfTestMarker(t, e)
		os.Remove(hook)
		fh := fakehost.New()
		oracle, toolset, cwd, err := native.NativeToolsForTest(e.g, fh)
		if err != nil {
			t.Fatal(err)
		}
		rd, ok := toolset[0].(*tools.Read)
		if cwd != canonical || oracle.Cwd != canonical || !ok || rd.Root() != canonical {
			t.Fatalf("cwd %q, oracle cwd %q, read root %v; want all %q", cwd, oracle.Cwd, rd, canonical)
		}
		ctx := context.Background()
		if out, isErr := toolset[0].Run(ctx, []byte(`{"path":"notes.txt"}`)); isErr || !strings.Contains(out, "alpha beta") {
			t.Fatalf("control read: %q", out)
		}
		move()
		for i, in := range []string{`{"path":"notes.txt"}`, `{"pattern":"alpha"}`, `{"command":"echo hi"}`} {
			out, isErr := toolset[i].Run(ctx, []byte(in))
			if !isErr || !strings.Contains(out, moved) {
				t.Errorf("%s after the move: %q, want refused with %q", toolset[i].Name(), out, moved)
			}
		}
		if d := oracle.Check(ctx, "shell", []byte(`{"command":"echo hi"}`)); d.Allow || !strings.Contains(d.Reason, moved) {
			t.Errorf("oracle after the move: %+v, want denied with %q", d, moved)
		}
		if exists(hook) || len(fh.Starts) != 0 {
			t.Fatalf("after the move: hook ran %v, %d host starts; want none", exists(hook), len(fh.Starts))
		}
	})
}
