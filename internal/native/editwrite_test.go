package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/reviewgate"
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
	names, err := native.ToolNamesForTest(e.g, fakehost.New(), false, state)
	if err != nil || strings.Join(names, ",") != "read,grep,shell" {
		t.Fatalf("without the gate: %v %v", names, err)
	}
	names, err = native.ToolNamesForTest(e.g, fakehost.New(), true, state)
	if err != nil || strings.Join(names, ",") != "read,grep,shell,edit,write" {
		t.Fatalf("with the gate: %v %v", names, err)
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
	t.Setenv("FABRIC_STATE_DIR", t.TempDir())
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
