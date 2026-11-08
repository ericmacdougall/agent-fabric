package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/native"
	"fabric/internal/reviewgate"
)

const reviewToolInput = `{"command":"echo reviewed"}`

func reviewPolicy(safe bool) reviewgate.Policy {
	tool := reviewgate.ToolPolicy{Allow: true, Risk: reviewgate.TierLow, Reversibility: reviewgate.Reversible, HumanRequired: true}
	if safe {
		tool.HumanRequired = false
		tool.AllowTaintedSideEffects = true
	}
	return reviewgate.Policy{Version: reviewgate.PolicyV1, ModelMaxTier: reviewgate.TierLow, Tools: map[string]reviewgate.ToolPolicy{
		"shell": tool,
	}}
}

func gatedShell(t *testing.T, r *rig, mode reviewgate.RolloutMode, safe bool, tool agent.Tool) **reviewgate.GatedTool {
	t.Helper()
	var gated *reviewgate.GatedTool
	r.cfgMod = func(c *agent.Config) {
		var err error
		gated, err = reviewgate.NewGatedTool(tool, reviewgate.NewPostgresStore(r.st.Pool()), reviewgate.Config{
			Policy: reviewPolicy(safe), Mode: mode, StateReader: native.AttemptStateReader{Pool: r.st.Pool()},
		}, reviewgate.AgentToolConfig{
			MissionID: string(missionID), SessionID: string(attemptID), ProposerID: "agent:" + string(attemptID),
			OriginalTask: "do the task",
		})
		if err != nil {
			t.Fatal(err)
		}
		c.Tools = []agent.Tool{gated}
	}
	return &gated
}

func runReviewLoop(t *testing.T, r *rig, want string) agent.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := r.loop().Run(ctx)
	if err != nil || out.Kind != want {
		t.Fatalf("review loop: out=%+v err=%v, want %s", out, err, want)
	}
	return out
}

func onlyReviewHash(t *testing.T, r *rig) string {
	t.Helper()
	var hash string
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT request_hash FROM review_gate_requests`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func approveReview(t *testing.T, r *rig, hash string) {
	t.Helper()
	op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(r.st.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := op.Approve(ctx, reviewgate.Approval{RequestHash: hash, OperatorID: "op:integration", Reason: "reviewed exact call", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func replayReviewCall(t *testing.T, gated *reviewgate.GatedTool) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(agent.WithToolCallContext(context.Background(), agent.ToolCallContext{ID: "toolu_review", MessageIndex: 1}), 8*time.Second)
	defer cancel()
	return gated.Run(ctx, json.RawMessage(reviewToolInput))
}

func TestReviewGateActualLoopApprovalReplayExecutesOnce(t *testing.T) {
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	gatedShell(t, r, reviewgate.Enforce, false, &fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs})
	paused := runReviewLoop(t, r, agent.OutcomeApprovalRequired)
	if len(r.runs) != 0 {
		t.Fatalf("side effect ran before approval: %v", r.runs)
	}
	hash := onlyReviewHash(t, r)
	if len(r.srv.Requests()) != 1 || !strings.Contains(paused.Note, "approval_required") || !strings.Contains(paused.Note, hash) {
		t.Fatal("durable structured approval result did not reach the agent loop")
	}
	var state string
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT state FROM agent_tool_calls WHERE attempt_id = $1`, attemptID).Scan(&state); err != nil || state != "PENDING" {
		t.Fatalf("paused call state=%q err=%v", state, err)
	}
	if err := r.db.FinishReviewedTool(context.Background(), attemptID, fence, "toolu_review", []byte("forge completion"), false); err == nil {
		t.Fatal("database allowed a side-effecting call to finish before approval")
	}
	approveReview(t, r, hash)
	runReviewLoop(t, r, agent.OutcomeDone)
	if len(r.runs) != 1 || len(r.srv.Requests()) != 2 {
		t.Fatalf("approved loop replay: runs=%v requests=%d", r.runs, len(r.srv.Requests()))
	}
	runReviewLoop(t, r, agent.OutcomeDone)
	if len(r.runs) != 1 || len(r.srv.Requests()) != 2 {
		t.Fatalf("terminal replay duplicated execution: runs=%v requests=%d", r.runs, len(r.srv.Requests()))
	}
}

func TestReviewGateActualLoopStaleStateBlocksApprovedReplay(t *testing.T) {
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	gatedShell(t, r, reviewgate.Enforce, false, &fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs})
	runReviewLoop(t, r, agent.OutcomeApprovalRequired)
	oldHash := onlyReviewHash(t, r)
	approveReview(t, r, oldHash)
	if _, err := r.st.Pool().Exec(context.Background(), `UPDATE attempts SET host = 'h_changed' WHERE id = $1`, attemptID); err != nil {
		t.Fatal(err)
	}
	paused := runReviewLoop(t, r, agent.OutcomeApprovalRequired)
	if !strings.Contains(paused.Note, "approval_required") || strings.Contains(paused.Note, oldHash) || len(r.runs) != 0 {
		t.Fatalf("stale approval was reused: out=%+v runs=%v", paused, r.runs)
	}
}

func TestReviewGateActualLoopShadowNeverExecutes(t *testing.T) {
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	gatedShell(t, r, reviewgate.Shadow, true, &fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs})
	runReviewLoop(t, r, agent.OutcomeDone)
	if len(r.runs) != 0 {
		t.Fatalf("shadow executed the tool: %v", r.runs)
	}
	if len(r.srv.Requests()) != 2 || !strings.Contains(string(r.srv.Requests()[1].Body), "shadow_no_execution") {
		t.Fatal("shadow result was not returned to the loop")
	}
}

type panicReviewTool struct{ calls atomic.Int32 }

func (p *panicReviewTool) Name() string        { return "shell" }
func (p *panicReviewTool) SideEffecting() bool { return true }
func (p *panicReviewTool) Def() json.RawMessage {
	return json.RawMessage(`{"name":"shell","input_schema":{"type":"object"}}`)
}
func (p *panicReviewTool) Run(context.Context, json.RawMessage) (string, bool) {
	p.calls.Add(1)
	panic("ambiguous tool crash")
}

func TestReviewGatePanicIsUnknownAndNeverRetries(t *testing.T) {
	tool := &panicReviewTool{}
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	gated := gatedShell(t, r, reviewgate.Enforce, true, tool)
	runReviewLoop(t, r, agent.OutcomeDone)
	if tool.calls.Load() != 1 || !strings.Contains(string(r.srv.Requests()[1].Body), "unknown_outcome") {
		t.Fatalf("panic did not become UNKNOWN: calls=%d", tool.calls.Load())
	}
	out, isError := replayReviewCall(t, *gated)
	if !isError || !strings.Contains(out, "unknown_outcome") || tool.calls.Load() != 1 {
		t.Fatalf("UNKNOWN retried: out=%q error=%v calls=%d", out, isError, tool.calls.Load())
	}
}

func TestReviewGateCrashAfterConfirmDoesNotDuplicate(t *testing.T) {
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	gatedShell(t, r, reviewgate.Enforce, true, &fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs})
	r.crashOnce("before_finish_tool")
	_, err, died := r.life()
	if err != nil || died == nil || died.point != "before_finish_tool" {
		t.Fatalf("crash injection: err=%v died=%+v", err, died)
	}
	if len(r.runs) != 1 {
		t.Fatalf("test setup did not execute once: %v", r.runs)
	}
	runReviewLoop(t, r, agent.OutcomeDone)
	if len(r.runs) != 1 || len(r.srv.Requests()) != 2 {
		t.Fatalf("confirmed crash replay duplicated work: runs=%v requests=%d", r.runs, len(r.srv.Requests()))
	}
}
