package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/reviewgate"
	"fabric/internal/storetest"
	"fabric/internal/tools/fakehost"
)

func reviewMarkerEnv(t *testing.T, config native.ReviewGateConfig) (*env, string) {
	t.Helper()
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_gate", "shell", `{"command":"touch reviewed.marker"}`, 100, 20)},
		fakeapi.Step{Events: fakeapi.Text("finished", 120, 10)},
	)
	e.g.ReviewGate = config
	e.g.ShellAllow = []string{`^touch [A-Za-z0-9_.-]+$`}
	return e, filepath.Join(e.g.Worktree, "reviewed.marker")
}

func TestNativeReviewGateDefaultShadowDoesNotExecute(t *testing.T) {
	// A read-only call needs no human, so shadow mode reaches its no-execution
	// outcome. (A shell call now always needs a human, goodqa#15867, and would
	// pause for one instead.)
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_gate", "read", `{"path":"shadow.txt"}`, 100, 20)},
		fakeapi.Step{Events: fakeapi.Text("finished", 120, 10)},
	)
	if err := os.WriteFile(filepath.Join(e.g.Worktree, "shadow.txt"), []byte("CONTENTS-OF-THE-SHADOW-READ"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.g.ReviewGate = native.ReviewGateConfig{}
	st := openNativeStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := native.Run(ctx, st, e.g, gateOpts(true)); err != nil {
		t.Fatal(err)
	}
	requests := e.srv.Requests()
	if len(requests) != 2 || !strings.Contains(string(requests[1].Body), "shadow_no_execution") {
		t.Fatal("missing shadow_no_execution tool result")
	}
	if strings.Contains(string(requests[1].Body), "CONTENTS-OF-THE-SHADOW-READ") {
		t.Fatal("the shadow read ran")
	}
}

func TestNativeReviewGateApprovalWakesAndResumesExactCallOnce(t *testing.T) {
	e, marker := reviewMarkerEnv(t, native.ReviewGateConfig{Mode: "enforce"})
	e.g.RestartCap = 1 // an approval wake is the same process, not a restart
	st := openNativeStore(t)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	workerHost := fakehost.New()
	runOpts := gateOpts(true)
	runOpts.Starter = workerHost
	runOpts.ReviewDSN = appDSN
	t.Setenv("FABRIC_REVIEW_DSN", appDSN) // must never leak into a tool worker
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	type runResult struct {
		receipt *native.Receipt
		err     error
	}
	finished := make(chan runResult, 1)
	go func() {
		receipt, err := native.Run(ctx, st, e.g, runOpts)
		finished <- runResult{receipt: receipt, err: err}
	}()
	var hash string
	// Leave time for the real multi-hook oracle self-test on loaded hosts while
	// retaining the same 8 s run context and 10 s package deadline.
	deadline := time.Now().Add(6 * time.Second)
	for hash == "" && time.Now().Before(deadline) {
		select {
		case result := <-finished:
			t.Fatalf("native run stopped before its approval wait: receipt=%+v err=%v", result.receipt, result.err)
		default:
		}
		_ = st.Pool().QueryRow(ctx, `SELECT request_hash FROM review_gate_requests WHERE state = 'AWAITING_HUMAN'`).Scan(&hash)
		if hash == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if hash == "" {
		select {
		case result := <-finished:
			t.Fatalf("native run stopped without an approval checkpoint: receipt=%+v err=%v", result.receipt, result.err)
		default:
		}
		var states string
		_ = st.Pool().QueryRow(ctx, `SELECT coalesce(string_agg(state, ','), 'none') FROM review_gate_requests`).Scan(&states)
		t.Fatalf("native run did not durably checkpoint its approval wait: states=%s requests=%d", states, len(e.srv.Requests()))
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tool executed before approval: %v", err)
	}
	if len(e.srv.Requests()) != 1 {
		t.Fatal("native pause did not preserve the pending call")
	}
	select {
	case result := <-finished:
		t.Fatalf("native run returned instead of waiting for approval: %+v", result)
	default:
	}
	op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Approve(ctx, reviewgate.Approval{RequestHash: hash, OperatorID: "op:native-test", Reason: "exact native call reviewed", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var result runResult
	select {
	case result = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not automatically resume the native run")
	}
	if result.err != nil || result.receipt.Outcome != "done" || result.receipt.Starts != 1 {
		t.Fatalf("approved resume: receipt=%+v err=%v", result.receipt, result.err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved tool did not execute: %v", err)
	}
	if len(e.srv.Requests()) != 2 {
		t.Fatalf("approved resume sent %d requests", len(e.srv.Requests()))
	}
	if len(workerHost.Starts) == 0 {
		t.Fatal("approved native tool did not reach the fake worker")
	}
	for _, start := range workerHost.Starts {
		for _, entry := range start.Env {
			if strings.HasPrefix(entry, "FABRIC_REVIEW_DSN=") {
				t.Fatal("app-role review DSN leaked into tool worker environment")
			}
		}
	}
	if _, err := native.Run(ctx, st, e.g, runOpts); err != nil {
		t.Fatal(err)
	}
	if len(e.srv.Requests()) != 2 {
		t.Fatal("terminal native replay repeated provider or tool work")
	}
}

func TestNativeReviewGateRequiresSeparateReviewDSN(t *testing.T) {
	e := newEnv(t)
	st := openNativeStore(t)
	o := gateOpts(true)
	o.ReviewDSN = ""
	if _, err := native.Run(context.Background(), st, e.g, o); err == nil || !strings.Contains(err.Error(), "FABRIC_REVIEW_DSN") {
		t.Fatalf("missing review DSN error = %v", err)
	}
}

func TestNativeReviewGateConfigValidation(t *testing.T) {
	e := newEnv(t)
	e.g.ReviewGate.Mode = "automatic"
	if err := e.g.Validate(); err == nil {
		t.Fatal("unknown review mode accepted")
	}
	e.g.ReviewGate = native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"not-a-tool"}}
	if err := e.g.Validate(); err == nil {
		t.Fatal("unknown safe tool accepted")
	}
	e.g.ReviewGate = native.ReviewGateConfig{Mode: "enforce", SafeTools: []string{"shell"}}
	if err := e.g.Validate(); err == nil || !strings.Contains(err.Error(), "edit") {
		t.Fatalf("shell accepted as a safe tool: %v", err)
	}
}

func TestNativeReviewGateApprovalCannotReviveLostLease(t *testing.T) {
	e, marker := reviewMarkerEnv(t, native.ReviewGateConfig{Mode: "enforce"})
	st := openNativeStore(t)
	st.Lease = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := native.Run(ctx, st, e.g, gateOpts(true))
		finished <- err
	}()
	var hash string
	deadline := time.Now().Add(6 * time.Second)
	for hash == "" && time.Now().Before(deadline) {
		select {
		case err := <-finished:
			t.Fatalf("native run stopped before its review wait: %v", err)
		default:
		}
		_ = st.Pool().QueryRow(ctx, `SELECT request_hash FROM review_gate_requests WHERE state = 'AWAITING_HUMAN'`).Scan(&hash)
		if hash == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if hash == "" {
		var states string
		scanErr := st.Pool().QueryRow(ctx, `SELECT coalesce(string_agg(state, ','), 'none') FROM review_gate_requests`).Scan(&states)
		select {
		case err := <-finished:
			t.Fatalf("native run stopped before its review wait: states=%s scan=%v err=%v", states, scanErr, err)
		default:
			t.Fatalf("native run did not reach its review wait: states=%s scan=%v requests=%d", states, scanErr, len(e.srv.Requests()))
		}
	}
	restore := native.SetHeartbeatFault(func() error { return domain.ErrInvalidTransition })
	defer restore()
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "heartbeat refused") || !strings.Contains(err.Error(), "nothing settled") {
			t.Fatalf("lost lease did not stop the waiter safely: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lease failure did not stop the approval wait")
	}
	op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Approve(ctx, reviewgate.Approval{RequestHash: hash, OperatorID: "op:late", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late approval executed after lease loss: %v", err)
	}
	if len(e.srv.Requests()) != 1 {
		t.Fatal("late approval issued another provider request")
	}
}
