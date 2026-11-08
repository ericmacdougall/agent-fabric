package reviewgate

import (
	"context"
	"strings"
	"testing"
)

// Migration 0037: a second request for a call that already has a dispatched or
// settled one cannot be stored in those states, whatever its hash. The guard
// triggers are switched off so the index itself is what is under test.
func TestCallHandleIsUniqueAcrossDispatchedAndSettledRequests(t *testing.T) {
	f := newRunnerReviewFixture(t)
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "once"}}, nil
	})
	g := f.gate(t, false, executor)
	d, err := g.Submit(f.ctx, "agent:r_runner_review", f.proposal)
	if err != nil {
		t.Fatal(err)
	}
	if d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Dispatch(f.ctx, d.RequestHash, f.proposal, "call-once"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Pool().Exec(f.ctx, `ALTER TABLE review_gate_requests DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.owner.Pool().Exec(context.Background(), `ALTER TABLE review_gate_requests ENABLE TRIGGER USER`) //nolint:errcheck
	})
	clone := func(hash, state, key string) error {
		_, err := f.owner.Pool().Exec(f.ctx, `INSERT INTO review_gate_requests
			(request_hash, mission_id, session_id, tool, proposer_id, policy_version, policy_digest, mode,
			 canonical_proposal, state, dispatch_idem_key, dispatch_fence)
			SELECT $1, mission_id, session_id, tool, proposer_id, policy_version, policy_digest, mode,
			 canonical_proposal, $2, $3, dispatch_fence FROM review_gate_requests WHERE request_hash = $4`,
			hash, state, key, d.RequestHash)
		return err
	}
	// Control: a PENDING request for the same call is not constrained.
	if err := clone(strings.Repeat("a", 64), "PENDING", "k-pending"); err != nil {
		t.Fatalf("control: a PENDING duplicate was refused: %v", err)
	}
	for i, state := range []string{"DISPATCHED", "UNKNOWN", "CONFIRMED", "FAILED_DEFINITE"} {
		err := clone(strings.Repeat(string(rune('b'+i)), 64), state, "k-"+state)
		if reviewSQLState(err) != "23505" || !strings.Contains(err.Error(), "review_gate_call_once") {
			t.Errorf("duplicate %s request for one call: %v, want unique violation review_gate_call_once", state, err)
		}
	}
}
