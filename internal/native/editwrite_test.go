package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/reviewgate"
	"fabric/internal/store"
	"fabric/internal/storetest"
	"fabric/internal/tools/fakehost"
)

func TestFileToolsGateRule(t *testing.T) {
	for _, c := range []struct {
		name string
		flag bool
		cfg  native.ReviewGateConfig
		want bool
	}{
		{"flag off, enforce", false, native.ReviewGateConfig{Mode: "enforce"}, false},
		{"flag on, no mode", true, native.ReviewGateConfig{}, false},
		{"flag on, safe_tools only", true, native.ReviewGateConfig{SafeTools: []string{"edit"}}, false},
		{"flag on, shadow", true, native.ReviewGateConfig{Mode: "shadow"}, false},
		{"flag on, evaluate", true, native.ReviewGateConfig{Mode: "evaluate"}, true},
		{"flag on, enforce", true, native.ReviewGateConfig{Mode: "enforce"}, true},
	} {
		if got := native.FileToolsEnabled(c.flag, c.cfg); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestNativeToolsetEditWrite(t *testing.T) {
	e := newEnv(t)
	state := t.TempDir()
	native.AllowStateDirUnderForTest(t, state)
	names, err := native.ToolNamesForTest(e.g, fakehost.New(), false, state)
	if err != nil || strings.Join(names, ",") != "read,grep,shell" {
		t.Fatalf("without the gate: %v %v", names, err)
	}
	names, err = native.ToolNamesForTest(e.g, fakehost.New(), true, state)
	if err != nil || strings.Join(names, ",") != "read,grep,shell,edit,write" {
		t.Fatalf("with the gate: %v %v", names, err)
	}
	// edit and write never exist without the pre-image store (F1)
	if _, err := native.ToolNamesForTest(e.g, fakehost.New(), true, ""); err == nil {
		t.Fatal("edit and write built without a state directory")
	}
	// the spill and pre-image directories are runner-only: under the state dir, 0700, never under RunDir
	for _, d := range []string{"spill", "preimage"} {
		st, err := os.Stat(filepath.Join(state, d, e.g.Attempt))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s dir: %v %v", d, st, err)
		}
	}
}

func TestEditWritePolicyIsPerCallReview(t *testing.T) {
	p, err := native.PolicyFor(native.ReviewGateConfig{Mode: "enforce"})
	if err != nil || !p["edit"] || !p["write"] {
		t.Fatalf("edit and write must need a human by default: %v %v", p, err)
	}
	p, err = native.PolicyFor(native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"edit", "write"}})
	if err != nil || p["edit"] || p["write"] {
		t.Fatalf("safe_tools is the opt-out: %v %v", p, err)
	}
}

const editCall = `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"ALPHA"}]}`

func editEnv(t *testing.T, cfg native.ReviewGateConfig) (*env, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("FABRIC_STATE_DIR", state)
	native.AllowStateDirUnderForTest(t, state) // go test temp dirs are under the temp roots
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_edit01", "edit", editCall, 100, 20)},
		fakeapi.Step{Events: fakeapi.Text("finished", 120, 10)},
	)
	e.g.ReviewGate = cfg
	return e, filepath.Join(e.g.Worktree, "notes.txt")
}

func firstBody(e *env) string { return string(e.srv.Requests()[0].Body) }

func TestEditWriteAbsentWithoutGate(t *testing.T) {
	// the flag off, and the flag on with no explicit mode: the tools do not exist
	for _, c := range []struct {
		name string
		gate bool
		cfg  native.ReviewGateConfig
	}{
		{"flag off", false, native.ReviewGateConfig{}},
		{"flag on, safe_tools only", true, native.ReviewGateConfig{SafeTools: []string{"edit"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, file := editEnv(t, c.cfg)
			o := opts(true)
			var st = openNativeStore(t)
			if c.gate {
				o = gateOpts(true)
				approveSafeTools(t, st, e.g, "op:test")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if _, err := native.Run(ctx, st, e.g, o); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(firstBody(e), `"name":"edit"`) || strings.Contains(firstBody(e), `"name":"write"`) {
				t.Fatal("the tool list names edit or write")
			}
			if b := string(e.srv.Requests()[1].Body); !strings.Contains(b, "unknown tool") {
				t.Fatalf("a model asking for edit must get the unknown-tool result: %s", b)
			}
			if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
				t.Fatalf("file changed: %q", got)
			}
		})
	}
}

func TestEditShadowAbsent(t *testing.T) {
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "shadow"})
	st := openNativeStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, gateOpts(true)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(firstBody(e), `"name":"edit"`) {
		t.Fatal("shadow mode names edit")
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestEditEvaluateNeverExecutes(t *testing.T) {
	// safe_tools so no human is needed: the gate then records the evaluate outcome
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "evaluate", SafeTools: []string{"edit"}})
	st := openNativeStore(t)
	approveSafeTools(t, st, e.g, "op:test")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, gateOpts(true)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstBody(e), `"name":"edit"`) {
		t.Fatal("evaluate mode must register edit")
	}
	if b := string(e.srv.Requests()[1].Body); !strings.Contains(b, "evaluate_no_execution") {
		t.Fatalf("no evaluate_no_execution result: %s", b)
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
		t.Fatalf("evaluate mode touched the file: %q", got)
	}
	var n int
	_ = st.Pool().QueryRow(ctx, `SELECT count(*) FROM review_gate_audit WHERE event = 'EVALUATE_NO_EXECUTION'`).Scan(&n)
	if n == 0 {
		t.Fatal("no EVALUATE_NO_EXECUTION audit row")
	}
}

func TestReviewGateEditWrite(t *testing.T) {
	// enforce, no safe_tools: the edit pauses, is applied only after approval,
	// and its result carries both digests
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "enforce"})
	e.g.RestartCap = 1
	st := openNativeStore(t)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	type res struct {
		r   *native.Receipt
		err error
	}
	done := make(chan res, 1)
	go func() { r, err := native.Run(ctx, st, e.g, o); done <- res{r, err} }()
	var hash string
	for deadline := time.Now().Add(8 * time.Second); hash == "" && time.Now().Before(deadline); {
		_ = st.Pool().QueryRow(ctx, `SELECT request_hash FROM review_gate_requests WHERE state = 'AWAITING_HUMAN' AND tool = 'edit'`).Scan(&hash)
		if hash == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if hash == "" {
		t.Fatal("the edit did not pause for review")
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
		t.Fatalf("edit applied before approval: %q", got)
	}
	op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Approve(ctx, reviewgate.Approval{RequestHash: hash, OperatorID: "op:edit-test", Reason: "exact edit reviewed", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.r.Outcome != "done" {
		t.Fatalf("%+v %v", r.r, r.err)
	}
	if got, _ := os.ReadFile(file); string(got) != "ALPHA beta\n" {
		t.Fatalf("approved edit not applied: %q", got)
	}
	if b := string(e.srv.Requests()[1].Body); !strings.Contains(b, "post_sha256") {
		t.Fatalf("result lacks the digests: %s", b)
	}
}

func TestReviewGateEditSafeToolsRunsWithoutPause(t *testing.T) {
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"edit", "write"}})
	st := openNativeStore(t)
	approveSafeTools(t, st, e.g, "op:test")
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, o); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); string(got) != "ALPHA beta\n" {
		t.Fatalf("safe_tools edit not applied: %q", got)
	}
	var n int
	_ = st.Pool().QueryRow(ctx, `SELECT count(*) FROM review_gate_requests WHERE state = 'AWAITING_HUMAN'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d requests paused", n)
	}
}

func TestEditAfterRestartDoesNotLeakSpill(t *testing.T) {
	// a finished attempt removes its spill directory
	e, _ := editEnv(t, native.ReviewGateConfig{})
	state := os.Getenv("FABRIC_STATE_DIR")
	st := openNativeStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, opts(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "spill", e.g.Attempt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spill directory outlived the attempt: %v", err)
	}
}

// C5 (goodqa#15865): edit and write are not built over a state directory the
// confined child can reach. The test override is exercised in the other
// tests; here it is off, so the production rule applies to t.TempDir().
func TestNativeToolsetRefusesChildReachableStateDir(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ name, dir string }{
		{"under the system temp roots", t.TempDir()},
		{"under the worktree", filepath.Join(e.g.Worktree, "state")},
		{"under the run dir", filepath.Join(e.g.RunDir, "state")},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := native.ToolNamesForTest(e.g, fakehost.New(), true, c.dir)
			if err == nil || !strings.Contains(err.Error(), "child can write") {
				t.Fatalf("edit and write built over %s: %v", c.dir, err)
			}
			if _, serr := os.Stat(filepath.Join(c.dir, "preimage")); serr == nil {
				t.Fatalf("a pre-image directory was created under %s", c.dir)
			}
		})
	}
	// without edit and write nothing is at stake: the read-only toolset builds
	if _, err := native.ToolNamesForTest(e.g, fakehost.New(), false, t.TempDir()); err != nil {
		t.Fatalf("read-only toolset refused a temp state dir: %v", err)
	}
}

// The test override reaches only the temp-root clause, and only for the path
// the test names.
func TestStateDirTestOverrideIsNarrow(t *testing.T) {
	e := newEnv(t)
	state := t.TempDir()
	native.AllowStateDirUnderForTest(t, state)
	if err := native.CheckStateDirForTest(state, e.g.Worktree, e.g.RunDir, nil); err != nil {
		t.Fatalf("the exempted state dir is refused: %v", err)
	}
	if err := native.CheckStateDirForTest(t.TempDir(), e.g.Worktree, e.g.RunDir, nil); err == nil {
		t.Fatal("another temp dir was exempted")
	}
	if err := native.CheckStateDirForTest(filepath.Join(state, "x"), state, e.g.RunDir, nil); err == nil {
		t.Fatal("the worktree clause was exempted")
	}
	if err := native.CheckStateDirForTest(filepath.Join(state, "x"), e.g.Worktree, state, nil); err == nil {
		t.Fatal("the run-dir clause was exempted")
	}
	if err := native.CheckStateDirForTest(filepath.Join(state, "x"), e.g.Worktree, e.g.RunDir, []string{state}); err == nil {
		t.Fatal("an extra writable root was exempted")
	}
}

func TestDefaultStateDirIsAccepted(t *testing.T) {
	t.Setenv("FABRIC_STATE_DIR", "")
	def, err := native.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(def, filepath.Join(".local", "state", "fabric")) {
		t.Fatalf("default is %s", def)
	}
	e := newEnv(t)
	if err := native.CheckStateDirForTest(def, e.g.Worktree, e.g.RunDir, nil); err != nil {
		if strings.HasPrefix(def, os.TempDir()) {
			t.Skipf("home is under the temp root on this host: %v", err)
		}
		t.Fatalf("the default state dir is refused: %v", err)
	}
}

// approveSafeTools records, as an operator, the approval a grant's safe_tools
// needs (goodqa#15866).
func approveSafeTools(t *testing.T, st *store.Store, g native.Grant, actor string) reviewgate.AuditEntry {
	t.Helper()
	digest, err := native.SafeToolsPolicyDigest(g.ReviewGate)
	if err != nil {
		t.Fatal(err)
	}
	op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	e, err := op.ApproveSafeTools(context.Background(), reviewgate.SafeToolsApproval{MissionID: g.Mission, PolicyDigest: digest, OperatorID: actor, Reason: "test approval"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSafeToolsRefusedWithoutAnApproval(t *testing.T) {
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"edit", "write"}})
	st := openNativeStore(t)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := native.Run(ctx, st, e.g, o)
	if err == nil || !strings.Contains(err.Error(), "operator approval") || !strings.Contains(err.Error(), e.g.Mission) {
		t.Fatalf("a grant with safe_tools and no recorded approval started: %v", err)
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
		t.Fatalf("file changed: %q", got)
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Fatalf("%d provider requests were sent", n)
	}
}

func TestSafeToolsApprovalMustMatchMissionAndPolicy(t *testing.T) {
	e, _ := editEnv(t, native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"edit", "write"}})
	st := openNativeStore(t)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// an approval for a narrower policy (edit only): another digest
	narrower := e.g
	narrower.ReviewGate.SafeTools = []string{"edit"}
	approveSafeTools(t, st, narrower, "op:alice")
	// an approval for another mission, same policy
	op, _ := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
	digest, _ := native.SafeToolsPolicyDigest(e.g.ReviewGate)
	if _, err := op.ApproveSafeTools(ctx, reviewgate.SafeToolsApproval{MissionID: "some-other-mission", PolicyDigest: digest, OperatorID: "op:alice", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Run(ctx, st, e.g, o); err == nil || !strings.Contains(err.Error(), "operator approval") {
		t.Fatalf("an approval for another policy or mission was accepted: %v", err)
	}
}

func TestSafeToolsStartsWithAMatchingApprovalAndRecordsTheApprover(t *testing.T) {
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"edit", "write"}})
	st := openNativeStore(t)
	entry := approveSafeTools(t, st, e.g, "op:bob")
	if entry.Actor != "op:bob" || !strings.Contains(entry.Detail, "mission="+e.g.Mission) {
		t.Fatalf("approval record: %+v", entry)
	}
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, o); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); string(got) != "ALPHA beta\n" {
		t.Fatalf("approved safe_tools edit not applied: %q", got)
	}
	var actor, detail string
	if err := st.Pool().QueryRow(ctx, `SELECT actor, detail FROM review_gate_audit WHERE event = 'SAFE_TOOLS_APPROVED'`).Scan(&actor, &detail); err != nil {
		t.Fatal(err)
	}
	digest, _ := native.SafeToolsPolicyDigest(e.g.ReviewGate)
	if actor != "op:bob" || !strings.Contains(detail, "policy_digest="+digest) {
		t.Fatalf("audit row: %s %s", actor, detail)
	}
}

// goodqa#15867 (C2): safe_tools is edit and write only, and shell is not Reversible.
func TestSafeToolsNamesOnlyEditAndWrite(t *testing.T) {
	e := newEnv(t)
	for _, bad := range [][]string{{"shell"}, {"edit", "shell"}, {"read"}, {"grep"}, {"not-a-tool"}, {""}, {"Edit"}} {
		e.g.ReviewGate = native.ReviewGateConfig{Mode: "enforce", SafeTools: bad}
		err := e.g.Validate()
		if err == nil || !strings.Contains(err.Error(), "safe_tools may name only") {
			t.Errorf("safe_tools %q accepted or unclear: %v", bad, err)
		}
		if _, perr := native.PolicyFor(e.g.ReviewGate); perr == nil {
			t.Errorf("policy built for safe_tools %q", bad)
		}
	}
	e.g.ReviewGate = native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"write", "edit"}}
	if err := e.g.Validate(); err != nil {
		t.Fatalf("edit and write refused: %v", err)
	}
}

func TestShellIsNotLabelledReversible(t *testing.T) {
	r, err := native.ToolReversibility(native.ReviewGateConfig{Mode: "enforce"}, "shell")
	if err != nil || r == "reversible" || r == "no_side_effect" {
		t.Fatalf("shell is labelled %q (%v): a shell command has no recorded undo", r, err)
	}
	if r, _ := native.ToolReversibility(native.ReviewGateConfig{Mode: "enforce"}, "edit"); r != "reversible" {
		t.Fatalf("edit is labelled %q", r)
	}
}

// goodqa#15865 (C4), through the production wiring: the worktree's own .git is
// inside the tree the child can write, so a write over a clean, committed file
// still keeps a copy.
func TestNativeWriteKeepsCopyWhenGitdirIsInTheWorktree(t *testing.T) {
	e := newEnv(t)
	state := t.TempDir()
	native.AllowStateDirUnderForTest(t, state)
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = e.g.Worktree
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(e.g.Worktree, "c.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "c.txt")
	git("commit", "-q", "-m", "c0")
	ts, closeFn, err := native.ToolsetForTest(e.g, fakehost.New(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	out, isErr := ts["write"].Run(context.Background(), json.RawMessage(`{"path":"c.txt","content":"changed\n"}`))
	if isErr || !strings.Contains(out, "preimage_copy: pre-") || strings.Contains(out, "preimage_git_blob") {
		t.Fatalf("the clean file's bytes were left to a gitdir the child can write: %s", out)
	}
}

// The host profile's extra writable roots reach the check through Run.
func TestRunHonoursExtraWritable(t *testing.T) {
	e, file := editEnv(t, native.ReviewGateConfig{Mode: "enforce"})
	st := openNativeStore(t)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	o := gateOpts(true)
	o.ReviewDSN = appDSN
	o.ExtraWritable = []string{os.Getenv("FABRIC_STATE_DIR")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, o); err == nil || !strings.Contains(err.Error(), "child can write") {
		t.Fatalf("a state dir inside an extra writable root was accepted: %v", err)
	}
	if got, _ := os.ReadFile(file); string(got) != "alpha beta\n" {
		t.Fatalf("file changed: %q", got)
	}
}
