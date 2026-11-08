package reviewgate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForLiveDispatchReturnsForTerminalReplay(t *testing.T) {
	st, ctx := openTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	executor := executorFunc(func(ctx context.Context, _ DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "finished once"}}, nil
		case <-ctx.Done():
			return ExecutionResult{Outcome: Unknown}, ctx.Err()
		}
	})
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executor)
	g.lease = time.Second
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	first := make(chan error, 1)
	go func() {
		_, err := g.Dispatch(ctx, hash, p, "live-dispatch-wait")
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not start")
	}
	waited := make(chan error, 1)
	go func() { waited <- st.WaitForReviewDecision(ctx, hash) }()
	select {
	case err := <-waited:
		t.Fatalf("live dispatch wait returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal dispatch did not release waiter")
	}
	replay, err := g.Dispatch(ctx, hash, p, "live-dispatch-wait")
	if err != nil || replay.Outcome != Confirmed || calls.Load() != 1 {
		t.Fatalf("terminal replay: %+v calls=%d err=%v", replay, calls.Load(), err)
	}
}

func TestWaitForStaleApprovedRequestUntilRenewal(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	hash := awaitingRequest(t, ctx, g, testProposal("delete"))
	if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "op:short", ExpiresAt: time.Now().Add(20 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	p := testProposal("delete")
	_ = p // the waiter deliberately depends only on durable request/approval state
	waited := make(chan error, 1)
	go func() { waited <- st.WaitForReviewDecision(ctx, hash) }()
	select {
	case err := <-waited:
		t.Fatalf("stale approval produced a busy resume: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "op:renewed", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval renewal did not release stale-approval waiter")
	}
}
