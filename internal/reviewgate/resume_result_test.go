package reviewgate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type literalResultRedactor struct{ secret string }

func (r literalResultRedactor) Redact(s string) string {
	return strings.ReplaceAll(s, r.secret, "[REDACTED:test]")
}

func TestApprovalWakesWaiterAndCommittedDecisionNeedsNoNotification(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	hash := awaitingRequest(t, ctx, g, testProposal("delete"))
	waited := make(chan error, 1)
	go func() { waited <- st.WaitForReviewDecision(ctx, hash) }()
	if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "op:wake", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("committed approval did not wake the native-style waiter")
	}
	// Starting after COMMIT means no notification can be pending for this call.
	// The durable state check must return immediately rather than wait for a hint.
	immediate, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := st.WaitForReviewDecision(immediate, hash); err != nil {
		t.Fatalf("already-approved request waited for a lost notification: %v", err)
	}
}

func TestDurableResultIsRedactedBoundedAndReplayable(t *testing.T) {
	st, ctx := openTestStore(t)
	const secret = "result-secret-that-must-never-reach-sql"
	var calls atomic.Int32
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "before " + secret + " " + strings.Repeat("x", 400)}}, nil
	})
	g, err := NewGate(st, reviewerFunc(allowReview), executor, Config{
		Policy: corePolicy(), Mode: Enforce, StateReader: staticStateReader{},
		Redactor: literalResultRedactor{secret: secret}, ResultLimit: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	if _, err := g.Dispatch(ctx, hash, p, "result-once"); err != nil {
		t.Fatal(err)
	}
	first, err := g.LoadToolResult(ctx, hash)
	if err != nil || len(first.Output) != 256 || !first.Truncated || strings.Contains(first.Output, secret) || !strings.Contains(first.Output, "[REDACTED:test]") {
		t.Fatalf("redacted bounded result: len=%d truncated=%v err=%v", len(first.Output), first.Truncated, err)
	}
	var stored []byte
	if err := st.pool.QueryRow(ctx, `SELECT output FROM review_gate_results WHERE request_hash = $1`, hash).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 256 || strings.Contains(string(stored), secret) {
		t.Fatalf("raw durable bytes leaked or exceeded their bound: len=%d", len(stored))
	}

	// A new Gate has no executor cache. Terminal dispatch and result loading are
	// both replay-only and must not invoke its executor again.
	replay, err := NewGate(st, reviewerFunc(allowReview), executor, Config{
		Policy: corePolicy(), Mode: Enforce, StateReader: staticStateReader{}, Redactor: literalResultRedactor{secret: secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Dispatch(ctx, hash, p, "result-once"); err != nil {
		t.Fatal(err)
	}
	second, err := replay.LoadToolResult(ctx, hash)
	if err != nil || second != first || calls.Load() != 1 {
		t.Fatalf("cross-gate replay: equal=%v calls=%d err=%v", second == first, calls.Load(), err)
	}
}

func TestResultCommitFailureCannotConfirmOrRetryEffect(t *testing.T) {
	st, ctx := openTestStore(t)
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION reject_review_result() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected result storage failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `CREATE TRIGGER reject_review_result BEFORE INSERT ON review_gate_results
		FOR EACH ROW EXECUTE FUNCTION reject_review_result()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		st.pool.Exec(context.Background(), `DROP TRIGGER reject_review_result ON review_gate_results`) //nolint:errcheck
		st.pool.Exec(context.Background(), `DROP FUNCTION reject_review_result()`)                     //nolint:errcheck
	}()
	var calls atomic.Int32
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "effected stdout"}}, nil
	})
	g, err := NewGate(st, reviewerFunc(allowReview), executor, Config{
		Policy: corePolicy(), Mode: Enforce, StateReader: staticStateReader{}, Lease: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	if _, err := g.Dispatch(ctx, hash, p, "result-failure"); err == nil || !strings.Contains(err.Error(), "injected result storage failure") {
		t.Fatalf("result commit failure was not surfaced: %v", err)
	}
	var state RequestState
	if err := st.pool.QueryRow(ctx, `SELECT state FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&state); err != nil || state != StateDispatched {
		t.Fatalf("failed atomic result commit left state %q, err=%v", state, err)
	}
	time.Sleep(15 * time.Millisecond)
	if outcome, err := g.Recover(ctx, hash); err != nil || outcome != Unknown {
		t.Fatalf("crash-left dispatch recovery: %s %v", outcome, err)
	}
	if _, err := g.Dispatch(ctx, hash, p, "result-failure"); !errors.Is(err, ErrUnknown) || calls.Load() != 1 {
		t.Fatalf("UNKNOWN result-storage failure retried the effect: calls=%d err=%v", calls.Load(), err)
	}
}
