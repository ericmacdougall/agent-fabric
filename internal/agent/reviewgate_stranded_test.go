package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/reviewgate"
)

// stoppingTool records the side effect, then stops the loop context so the
// terminal settlement cannot commit and the request stays DISPATCHED, as when
// the process dies between execution and its terminal commit.
type stoppingTool struct {
	runs *int
	stop context.CancelFunc
}

func (c *stoppingTool) Name() string        { return "shell" }
func (c *stoppingTool) SideEffecting() bool { return true }
func (c *stoppingTool) Def() json.RawMessage {
	return json.RawMessage(`{"name":"shell","input_schema":{"type":"object"}}`)
}
func (c *stoppingTool) Run(context.Context, json.RawMessage) (string, bool) {
	*c.runs++
	if *c.runs == 1 && c.stop != nil {
		c.stop()
	}
	return "did it", false
}

// strandedDispatch leaves one request DISPATCHED for toolu_review with the side
// effect already run once, then optionally changes a fingerprinted column.
func strandedDispatch(t *testing.T, changeHost bool) (*rig, *int) {
	t.Helper()
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	runs := 0
	tool := &stoppingTool{runs: &runs}
	gatedShell(t, r, reviewgate.Enforce, true, tool)
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	tool.stop = stop
	_, err := r.loop().Run(ctx)
	stop()
	if err == nil {
		t.Fatal("expected the first life to fail after its context stopped")
	}
	var state string
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT state FROM review_gate_requests`).Scan(&state); err != nil || state != "DISPATCHED" || runs != 1 {
		t.Fatalf("setup: review state=%q runs=%d err=%v, want DISPATCHED after one run", state, runs, err)
	}
	if changeHost {
		if _, err := r.st.Pool().Exec(context.Background(), `UPDATE attempts SET host = 'h_changed' WHERE id = $1`, attemptID); err != nil {
			t.Fatal(err)
		}
	}
	return r, &runs
}

// A call whose dispatch never settled must never run a second time, even when
// the trusted-state fingerprint (and so the request hash) changed before the
// next life: the call handle, not the hash, identifies the side effect.
func TestStrandedDispatchedCallIsNeverRerunAfterFingerprintChange(t *testing.T) {
	r, runs := strandedDispatch(t, true)
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	out, err := r.loop().Run(ctx)
	if err != nil {
		t.Fatalf("second life: %v", err)
	}
	var requests int
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM review_gate_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if *runs != 1 || requests != 1 {
		t.Fatalf("side effect ran %d times with %d review requests for one tool_use_id (out=%+v), want 1 and 1", *runs, requests, out)
	}
	if out.Kind != agent.OutcomeApprovalRequired || out.Review == nil || out.Review.Status != "dispatch_in_progress" {
		t.Fatalf("stranded dispatch should pause as dispatch_in_progress: %+v", out)
	}
}

// Control: with no fingerprint change the same stranded call already paused correctly.
func TestStrandedDispatchedCallPausesWithoutFingerprintChange(t *testing.T) {
	r, runs := strandedDispatch(t, false)
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	out, err := r.loop().Run(ctx)
	if err != nil {
		t.Fatalf("second life: %v", err)
	}
	if *runs != 1 || out.Kind != agent.OutcomeApprovalRequired || out.Review == nil || out.Review.Status != "dispatch_in_progress" {
		t.Fatalf("control: runs=%d out=%+v, want 1 run and a dispatch_in_progress pause", *runs, out)
	}
}
