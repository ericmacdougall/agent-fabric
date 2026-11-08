package reviewgate

import (
	"context"
	"encoding/json"
	"testing"

	"fabric/internal/agent"
)

// A confirmed review whose provenance names the pending call but whose
// arguments differ from the call the model actually made must not settle that
// call: the runner may only collect an outcome for the exact arguments
// (goodqa#15235).
func TestRunnerCannotSettleCallWithDifferentArgumentsFromReview(t *testing.T) {
	f := newRunnerReviewFixture(t)
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "other args output"}}, nil
	})
	other := f.proposal
	other.Args = json.RawMessage(`{"path":"/etc/other","note":"different"}`)
	g := f.gate(t, false, executor)
	d, err := g.Submit(f.ctx, "agent:r_runner_review", other)
	if err != nil {
		t.Fatal(err)
	}
	if d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Dispatch(f.ctx, d.RequestHash, other, "different-args"); err != nil {
		t.Fatal(err)
	}
	db := &agent.DB{Pool: f.runnerPool}
	if err := db.FinishReviewedTool(f.ctx, "r_runner_review", 1, "toolu_runner", []byte("forged"), false); err == nil {
		t.Fatal("runner settled a call using a review of different arguments")
	}
	assertPendingRunnerCall(t, f)
}
