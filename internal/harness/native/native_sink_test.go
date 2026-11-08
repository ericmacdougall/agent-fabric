package native_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

func toolStep(call int, _ context.Context) (*agent.Result, error) {
	if call == 1 {
		return &agent.Result{
			Content:    blocks(text("starting"), map[string]any{"type": "tool_use", "id": "toolu_1", "name": "echo", "input": map[string]string{"cmd": "a"}}),
			StopReason: "tool_use", Stop: agent.StopTools, Usage: agent.Usage{Input: 10, Output: 5}, Model: "test-model", RequestSHA: "x",
			ToolUses: []agent.ToolUse{{ID: "toolu_1", Name: "echo", Input: json.RawMessage(`{"cmd":"a"}`)}},
		}, nil
	}
	return endResult(text("all done")), nil
}

// A run whose events cannot be recorded must stop acting: no further provider
// call, no tool run, and no tool call left half-recorded.
func TestNativeSinkFailureStopsActing(t *testing.T) {
	a, env := adapter(nil, t, toolStep)
	rec := &harnesstest.Recorder{FailAfter: 1} // the turn is recorded, the next event is not
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_sinkfail"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("outcome %q, want failed", f.Outcome)
	}
	if env.prov.calls != 1 {
		t.Fatalf("%d provider calls, want exactly the one already started", env.prov.calls)
	}
	if *env.runs != 0 {
		t.Fatalf("a tool ran %d times after events could no longer be recorded", *env.runs)
	}
	calls, err := env.db.ToolCalls(ctx, domain.AttemptID(attemptID))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if c.State == "EXECUTING" {
			t.Fatalf("tool call %s left EXECUTING", c.ID)
		}
	}
}

// If the turn cannot be recorded, the paid provider call is not made.
func TestNativeTurnNotRecordedMeansNoProviderCall(t *testing.T) {
	a, env := adapter(nil, t, toolStep)
	rec := &harnesstest.Recorder{FailNow: true}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_noturn"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("outcome %q", f.Outcome)
	}
	if env.prov.calls != 0 {
		t.Fatalf("%d provider calls made although the turn could not be recorded", env.prov.calls)
	}
}

// Two identical calls in one response keep their own ids: ids come from the
// loop's context, not from matching name and input.
func TestNativeToolCallIDsComeFromTheLoop(t *testing.T) {
	step := func(call int, _ context.Context) (*agent.Result, error) {
		if call == 1 {
			in := map[string]string{"cmd": "same"}
			return &agent.Result{
				Content: blocks(map[string]any{"type": "tool_use", "id": "toolu_a", "name": "echo", "input": in},
					map[string]any{"type": "tool_use", "id": "toolu_b", "name": "echo", "input": in}),
				StopReason: "tool_use", Stop: agent.StopTools, Usage: agent.Usage{Input: 1, Output: 1}, Model: "test-model", RequestSHA: "x",
				ToolUses: []agent.ToolUse{{ID: "toolu_a", Name: "echo", Input: json.RawMessage(`{"cmd":"same"}`)}, {ID: "toolu_b", Name: "echo", Input: json.RawMessage(`{"cmd":"same"}`)}},
			}, nil
		}
		return endResult(text("done")), nil
	}
	a, _ := adapter(nil, t, step)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_ids"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if f, _ := s.Wait(ctx); f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	var ok []string
	for _, e := range rec.Snapshot() {
		if e.Kind == harness.KindToolCall {
			var tc harness.ToolCall
			_ = json.Unmarshal(e.Payload, &tc)
			if tc.Status == "ok" {
				ok = append(ok, tc.ID)
			}
		}
	}
	if len(ok) != 2 || ok[0] != "toolu_a" || ok[1] != "toolu_b" {
		t.Fatalf("finished tool call ids %v, want [toolu_a toolu_b]", ok)
	}
}
