package reviewgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	fabricstore "fabric/internal/store"
)

var testDSN string

func TestMain(m *testing.M) {
	admin := os.Getenv("FABRIC_TEST_DSN")
	if admin == "" {
		os.Exit(m.Run()) // pure tests still run; each PostgreSQL test skips loudly
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin connect:", err)
		os.Exit(2)
	}
	name := fmt.Sprintf("reviewgate_t_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		fmt.Fprintln(os.Stderr, "create database:", err)
		os.Exit(2)
	}
	u, err := url.Parse(admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse DSN:", err)
		os.Exit(2)
	}
	u.Path = "/" + name
	testDSN = u.String()
	st, err := fabricstore.Open(ctx, testDSN)
	if err == nil {
		err = st.Migrate(ctx)
		st.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		pool.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`) //nolint:errcheck
		os.Exit(2)
	}
	code := m.Run()
	if _, err := pool.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
		fmt.Fprintln(os.Stderr, "drop database:", err)
	}
	pool.Close()
	os.Exit(code)
}

type reviewerFunc func(context.Context, ReviewInput) (ModelDecision, error)

func (f reviewerFunc) Review(ctx context.Context, in ReviewInput) (ModelDecision, error) {
	return f(ctx, in)
}

type executorFunc func(context.Context, DispatchRequest) (ExecutionResult, error)

func (f executorFunc) Execute(ctx context.Context, in DispatchRequest) (ExecutionResult, error) {
	return f(ctx, in)
}

type stateReaderFunc func(context.Context, string, string) (string, error)

func (f stateReaderFunc) CurrentFingerprint(ctx context.Context, mission, session string) (string, error) {
	return f(ctx, mission, session)
}

type staticStateReader struct{}

func (staticStateReader) CurrentFingerprint(context.Context, string, string) (string, error) {
	return "state-v1", nil
}

func allowReview(context.Context, ReviewInput) (ModelDecision, error) {
	return ModelDecision{Verdict: ReviewAllow, Reason: "fake reviewer reason with top-secret-payload"}, nil
}

func confirmedExecute(context.Context, DispatchRequest) (ExecutionResult, error) {
	return ExecutionResult{Outcome: Confirmed, Detail: "executor top-secret-payload"}, nil
}

func openTestStore(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	if testDSN == "" {
		t.Skip("FABRIC_TEST_DSN is not set: PostgreSQL integration test did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	st, err := Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SET LOCAL fabric.allow_truncate = 'on'",
		"TRUNCATE review_gate_approvals, review_gate_audit, review_gate_requests, review_gate_circuits RESTART IDENTITY CASCADE",
		"UPDATE review_gate_audit_head SET seq = 0, last_hash = '' WHERE id = 1",
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

func newTestGate(t *testing.T, st *PostgresStore, mode RolloutMode, reviewer ModelReviewer, executor Executor) *Gate {
	t.Helper()
	g, err := NewGate(st, reviewer, executor, Config{Policy: corePolicy(), StateReader: staticStateReader{}, Mode: mode, DenialThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var proposalSequence atomic.Int64

func testProposal(tool string) Proposal {
	n := proposalSequence.Add(1)
	p := coreProposal()
	p.SessionID = fmt.Sprintf("s_%d", n)
	p.Tool = tool
	p.Args = json.RawMessage(fmt.Sprintf(`{"path":"/tmp/%d","note":"top-secret-payload"}`, n))
	return p
}

func readyRequest(t *testing.T, ctx context.Context, g *Gate, p Proposal) string {
	t.Helper()
	d, err := g.Submit(ctx, "agent_1", p)
	if err != nil {
		t.Fatal(err)
	}
	d, err = g.Evaluate(ctx, d.RequestHash, "original task only")
	if err != nil || d.State != StateReady {
		t.Fatalf("ready evaluation: %+v, %v", d, err)
	}
	return d.RequestHash
}

func awaitingRequest(t *testing.T, ctx context.Context, g *Gate, p Proposal) string {
	t.Helper()
	d, err := g.Submit(ctx, "agent_1", p)
	if err != nil {
		t.Fatal(err)
	}
	d, err = g.Evaluate(ctx, d.RequestHash, "original task only")
	if err != nil || d.State != StateAwaitingHuman {
		t.Fatalf("awaiting evaluation: %+v, %v", d, err)
	}
	return d.RequestHash
}

func TestReviewerIsolationAndFailuresAreClosed(t *testing.T) {
	st, ctx := openTestStore(t)
	var seen ReviewInput
	reviewer := reviewerFunc(func(_ context.Context, in ReviewInput) (ModelDecision, error) {
		seen = in
		return ModelDecision{Verdict: ReviewAllow}, nil
	})
	g := newTestGate(t, st, Enforce, reviewer, executorFunc(confirmedExecute))
	p := testProposal("write")
	d, err := g.Submit(ctx, "agent_1", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Evaluate(ctx, d.RequestHash, "the original task"); err != nil {
		t.Fatal(err)
	}
	canonical, _ := CanonicalProposal(p)
	if seen.OriginalTask != "the original task" || string(seen.CanonicalProposal) != string(canonical) || seen.Risk.PolicyVersion != PolicyV1 {
		t.Fatalf("reviewer input mismatch: %+v", seen)
	}
	wire, _ := json.Marshal(seen)
	var fields map[string]json.RawMessage
	json.Unmarshal(wire, &fields) //nolint:errcheck
	if len(fields) != 3 || fields["original_task"] == nil || fields["canonical_proposal"] == nil || fields["risk"] == nil {
		t.Fatalf("reviewer gained an input channel: %s", wire)
	}

	for name, reviewer := range map[string]ModelReviewer{
		"error": reviewerFunc(func(context.Context, ReviewInput) (ModelDecision, error) {
			return ModelDecision{}, errors.New("fake reviewer unavailable")
		}),
		"panic": reviewerFunc(func(context.Context, ReviewInput) (ModelDecision, error) {
			panic("fake reviewer crash")
		}),
		"malformed": reviewerFunc(func(context.Context, ReviewInput) (ModelDecision, error) {
			return ModelDecision{Verdict: "permit-without-checks"}, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			g := newTestGate(t, st, Enforce, reviewer, executorFunc(confirmedExecute))
			p := testProposal("write")
			d, err := g.Submit(ctx, "agent_1", p)
			if err != nil {
				t.Fatal(err)
			}
			d, err = g.Evaluate(ctx, d.RequestHash, "task")
			if !errors.Is(err, ErrDenied) || d.State != StateDenied {
				t.Fatalf("failure was not closed: %+v, %v", d, err)
			}
		})
	}
}

func TestModelCannotDowngradeAndApprovalBinding(t *testing.T) {
	st, ctx := openTestStore(t)
	exec := executorFunc(confirmedExecute)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), exec)
	for _, p := range []Proposal{testProposal("delete"), testProposal("high"), func() Proposal {
		p := testProposal("write")
		p.Provenance = []ProvenanceHandle{{ID: "outside", Origin: OriginExternal, Tainted: false}}
		return p
	}()} {
		hash := awaitingRequest(t, ctx, g, p)
		if _, err := g.Dispatch(ctx, hash, p, "before-approval"); !errors.Is(err, ErrApprovalRequired) {
			t.Fatalf("model allow dispatched above its authority: %v", err)
		}
		if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "agent_1", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrDenied) {
			t.Fatalf("proposer self-approved: %v", err)
		}
		if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "operator_1", ExpiresAt: time.Now().Add(-time.Second)}); !errors.Is(err, ErrDenied) {
			t.Fatalf("expired approval accepted: %v", err)
		}
		approval := Approval{RequestHash: hash, OperatorID: "operator_1", ExpiresAt: time.Now().Add(time.Hour)}
		if _, err := g.Approve(ctx, approval); err != nil {
			t.Fatal(err)
		}
		if replay, err := g.Approve(ctx, approval); err != nil || !replay.Replayed {
			t.Fatalf("same-operator approval did not replay: %+v, %v", replay, err)
		}
		wrongOperator := approval
		wrongOperator.OperatorID = "agent_1"
		if _, err := g.Approve(ctx, wrongOperator); !errors.Is(err, ErrDenied) {
			t.Fatalf("wrong operator replayed another operator's approval: %v", err)
		}
		stale := p
		stale.Args = json.RawMessage(`{"path":"/different","note":"top-secret-payload"}`)
		if _, err := g.Dispatch(ctx, hash, stale, "stale-hash"); !errors.Is(err, ErrStale) {
			t.Fatalf("stale hash dispatch: %v", err)
		}
	}
}

func TestApprovalExpiryAtDispatch(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	p := testProposal("delete")
	hash := awaitingRequest(t, ctx, g, p)
	if _, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "operator_1", ExpiresAt: time.Now().Add(30 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := g.Dispatch(ctx, hash, p, "expired"); !errors.Is(err, ErrStale) {
		t.Fatalf("expired approval dispatched: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE review_gate_requests SET state = 'DISPATCHED', dispatch_idem_key = 'sql-expired',
		dispatch_fence = 1, lease_until = clock_timestamp() + interval '1 minute' WHERE request_hash = $1`, hash); err == nil {
		t.Fatal("SQL guard dispatched without a live approval")
	}
	if replay, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "operator_1", ExpiresAt: time.Now().Add(-time.Second)}); !errors.Is(err, ErrDenied) || replay.Replayed {
		t.Fatalf("expired approval replayed as live: %+v, %v", replay, err)
	}
	renewed, err := g.Approve(ctx, Approval{RequestHash: hash, OperatorID: "operator_2", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil || renewed.State != StateApproved || !renewed.Risk.HumanRequired {
		t.Fatalf("durable renewal: %+v, %v", renewed, err)
	}
	var approvals int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM review_gate_approvals WHERE request_hash = $1 AND policy_digest = $2`, hash, g.policyDigest).Scan(&approvals); err != nil || approvals != 2 {
		t.Fatalf("renewal was not append-only and policy bound: count=%d err=%v", approvals, err)
	}
	if result, err := g.Dispatch(ctx, hash, p, "renewed"); err != nil || result.Outcome != Confirmed {
		t.Fatalf("renewed approval did not dispatch: %+v, %v", result, err)
	}
}

func TestConcurrentDispatchCallsExecutorOnce(t *testing.T) {
	st, ctx := openTestStore(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return ExecutionResult{Outcome: Confirmed}, nil
	})
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), exec)
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]DispatchResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = g.Dispatch(ctx, hash, p, "one-business-action")
		}(i)
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	owners := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("dispatch %d: %v", i, errs[i])
		}
		if results[i].Owner {
			owners++
		}
	}
	if calls.Load() != 1 || owners != 1 {
		t.Fatalf("executor calls=%d owners=%d, want 1/1", calls.Load(), owners)
	}
	d, err := g.Inspect(ctx, hash)
	if err != nil || d.State != StateConfirmed {
		t.Fatalf("final state: %+v, %v", d, err)
	}
}

func TestAmbiguousExecutionIsUnknownAndNeverAutoRetries(t *testing.T) {
	st, ctx := openTestStore(t)
	var calls atomic.Int32
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed, Detail: "may have happened"}, errors.New("ambiguous connection loss")
	})
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), exec)
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	result, err := g.Dispatch(ctx, hash, p, "ambiguous")
	if !errors.Is(err, ErrUnknown) || result.Outcome != Unknown {
		t.Fatalf("ambiguous result: %+v, %v", result, err)
	}
	if _, err := g.Dispatch(ctx, hash, p, "ambiguous"); !errors.Is(err, ErrUnknown) || calls.Load() != 1 {
		t.Fatalf("UNKNOWN automatically retried: calls=%d err=%v", calls.Load(), err)
	}
	settled, err := g.Settle(ctx, Settlement{RequestHash: hash, Fence: 1, Outcome: Confirmed,
		Actor: "readback_1", Evidence: "readback top-secret-payload"})
	if err != nil || settled.State != StateConfirmed {
		t.Fatalf("explicit settlement: %+v, %v", settled, err)
	}
}

func TestCrashLeftDispatchRecoversUnknown(t *testing.T) {
	st, ctx := openTestStore(t)
	var calls atomic.Int32
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		panic("fake process crash")
	})
	g, err := NewGate(st, reviewerFunc(allowReview), exec, Config{Policy: corePolicy(), StateReader: staticStateReader{}, Mode: Enforce, Lease: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fake executor did not crash")
			}
		}()
		g.Dispatch(ctx, hash, p, "crash") //nolint:errcheck
	}()
	d, err := g.Inspect(ctx, hash)
	if err != nil || d.State != StateDispatched {
		t.Fatalf("crash did not leave DISPATCHED: %+v, %v", d, err)
	}
	time.Sleep(50 * time.Millisecond)
	recoverable, err := g.ListRecovery(ctx, 10)
	if err != nil || len(recoverable) != 1 || recoverable[0] != hash {
		t.Fatalf("recovery listing: %v, %v", recoverable, err)
	}
	outcome, err := g.Recover(ctx, hash)
	if err != nil || outcome != Unknown {
		t.Fatalf("recovery: %s, %v", outcome, err)
	}
	if late, err := g.finishExecution(ctx, hash, "crash", 1, Confirmed); !errors.Is(err, ErrUnknown) || late.Outcome != Unknown {
		t.Fatalf("late finish overwrote recovery: %+v, %v", late, err)
	}
	if state, _ := g.Inspect(ctx, hash); state.State != StateUnknown {
		t.Fatalf("late finish changed state to %s", state.State)
	}
	if _, err := g.Dispatch(ctx, hash, p, "crash"); !errors.Is(err, ErrUnknown) || calls.Load() != 1 {
		t.Fatalf("recovery retried execution: calls=%d err=%v", calls.Load(), err)
	}
}

func TestDenialCircuitBreakerPerSession(t *testing.T) {
	st, ctx := openTestStore(t)
	g, err := NewGate(st, reviewerFunc(func(context.Context, ReviewInput) (ModelDecision, error) {
		return ModelDecision{Verdict: ReviewDeny}, nil
	}), executorFunc(confirmedExecute), Config{Policy: corePolicy(), StateReader: staticStateReader{}, Mode: Enforce, DenialThreshold: 2})
	if err != nil {
		t.Fatal(err)
	}
	session := "shared_session"
	for i := 0; i < 2; i++ {
		p := testProposal("write")
		p.SessionID = session
		d, err := g.Submit(ctx, "agent_1", p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.Evaluate(ctx, d.RequestHash, "task"); !errors.Is(err, ErrDenied) {
			t.Fatalf("denial %d: %v", i, err)
		}
	}
	p := testProposal("write")
	p.SessionID = session
	if _, err := g.Submit(ctx, "agent_1", p); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open circuit did not refuse before insert: %v", err)
	}
	var inserted int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM review_gate_requests WHERE session_id = $1`, session).Scan(&inserted); err != nil || inserted != 2 {
		t.Fatalf("open-circuit submission was inserted: count=%d err=%v", inserted, err)
	}
	// Another session is independent.
	other := testProposal("write")
	d, err := g.Submit(ctx, "agent_1", other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Evaluate(ctx, d.RequestHash, "task"); !errors.Is(err, ErrDenied) {
		t.Fatalf("independent session: %v", err)
	}
}

func TestAuditChainAndSecretFreeMutationResistance(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	if _, err := g.Dispatch(ctx, hash, p, "audit"); err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Audit(ctx)
	if err != nil || len(entries) < 3 {
		t.Fatalf("audit entries: %d, %v", len(entries), err)
	}
	for _, e := range entries {
		if len(e.Detail) > MaxAuditText || strings.Contains(e.Detail, "top-secret-payload") || strings.Contains(e.Actor, "top-secret-payload") {
			t.Fatalf("secret or oversized audit text: %+v", e)
		}
	}
	page1, err := st.AuditPage(ctx, 0, 2)
	if err != nil || len(page1) != 2 {
		t.Fatalf("audit page 1: %d, %v", len(page1), err)
	}
	page2, err := st.AuditPage(ctx, page1[len(page1)-1].Seq, 2)
	if err != nil || len(page2) == 0 || page2[0].Seq <= page1[len(page1)-1].Seq {
		t.Fatalf("audit page 2: %+v, %v", page2, err)
	}
	if _, err := st.AuditPage(ctx, 0, 1001); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded audit page accepted: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE review_gate_audit SET detail = 'forged' WHERE seq = 1`); err == nil {
		t.Fatal("audit UPDATE was accepted")
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM review_gate_audit WHERE seq = 1`); err == nil {
		t.Fatal("audit DELETE was accepted")
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatalf("chain changed after rejected mutations: %v", err)
	}
}

func TestShadowAndEvaluateNeverExecute(t *testing.T) {
	st, ctx := openTestStore(t)
	var calls atomic.Int32
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed}, nil
	})
	for _, mode := range []RolloutMode{Shadow, Evaluate} {
		g := newTestGate(t, st, mode, reviewerFunc(allowReview), exec)
		p := testProposal("write")
		hash := readyRequest(t, ctx, g, p)
		result, err := g.Dispatch(ctx, hash, p, string(mode))
		if err != nil || result.Outcome != "" || result.Owner {
			t.Fatalf("%s dispatch: %+v, %v", mode, result, err)
		}
		d, _ := g.Inspect(ctx, hash)
		if d.State != StateReady {
			t.Fatalf("%s mutated request to %s", mode, d.State)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("non-enforcing modes made %d executor calls", calls.Load())
	}
}

func TestRunnerRoleCannotApproveOrRewrite(t *testing.T) {
	st, ctx := openTestStore(t)
	rolesStore, err := fabricstore.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer rolesStore.Close()
	if err := rolesStore.ApplyRoles(ctx, fabricstore.Roles{Prefix: "reviewgate_test"}, "", ""); err != nil {
		t.Fatal(err)
	}
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	p := testProposal("delete")
	hash := awaitingRequest(t, ctx, g, p)
	// Re-apply the migration's existing-role revocation after ApplyRoles'
	// catalog-wide grant. roles.go independently applies the same restriction.
	if _, err := st.pool.Exec(ctx, `SELECT fabric_review_gate_revoke_runner_reads()`); err != nil {
		t.Fatal(err)
	}
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE reviewgate_test_runner"); err != nil {
		t.Fatal(err)
	}
	_, approveErr := conn.Exec(ctx, `INSERT INTO review_gate_approvals
		(request_hash, operator_id, proposer_id, policy_digest, expires_at)
		VALUES ($1, 'operator_1', 'agent_1', $2, now() + interval '1 hour')`, hash, g.policyDigest)
	_, rewriteErr := conn.Exec(ctx, `UPDATE review_gate_requests SET state = 'APPROVED' WHERE request_hash = $1`, hash)
	var plaintext string
	readErr := conn.QueryRow(ctx, `SELECT canonical_proposal FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&plaintext)
	_, circuitErr := conn.Exec(ctx, `UPDATE review_gate_circuits SET open = false`)
	_, headErr := conn.Exec(ctx, `UPDATE review_gate_audit_head SET seq = 0 WHERE id = 1`)
	if approveErr == nil || rewriteErr == nil || readErr == nil || circuitErr == nil || headErr == nil {
		t.Fatalf("runner authority: approve=%v rewrite=%v read=%v circuit=%v head=%v", approveErr, rewriteErr, readErr, circuitErr, headErr)
	}
	if _, err := conn.Exec(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `TRUNCATE review_gate_circuits`); err == nil {
		t.Fatal("circuits TRUNCATE was accepted")
	}
	if _, err := st.pool.Exec(ctx, `TRUNCATE review_gate_audit_head`); err == nil {
		t.Fatal("audit head TRUNCATE was accepted")
	}
}

func TestDefiniteAndMalformedExecutorOutcomes(t *testing.T) {
	t.Run("definite failure", func(t *testing.T) {
		st, ctx := openTestStore(t)
		g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
			return ExecutionResult{Outcome: FailedDefinite}, nil
		}))
		p := testProposal("write")
		hash := readyRequest(t, ctx, g, p)
		result, err := g.Dispatch(ctx, hash, p, "definite")
		if err != nil || result.Outcome != FailedDefinite {
			t.Fatalf("definite executor result: %+v, %v", result, err)
		}
	})
	t.Run("malformed success claim", func(t *testing.T) {
		st, ctx := openTestStore(t)
		g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
			return ExecutionResult{}, nil
		}))
		p := testProposal("write")
		hash := readyRequest(t, ctx, g, p)
		result, err := g.Dispatch(ctx, hash, p, "malformed")
		if !errors.Is(err, ErrUnknown) || result.Outcome != Unknown {
			t.Fatalf("malformed executor result: %+v, %v", result, err)
		}
	})
}

func TestPolicyContentChangeBlocksApprovedDispatch(t *testing.T) {
	st, ctx := openTestStore(t)
	var calls atomic.Int32
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed}, nil
	})
	original := newTestGate(t, st, Enforce, reviewerFunc(allowReview), exec)
	p := testProposal("delete")
	hash := awaitingRequest(t, ctx, original, p)
	if _, err := original.Approve(ctx, Approval{RequestHash: hash, OperatorID: "operator_1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	tightened := corePolicy()
	tool := tightened.Tools["delete"]
	tool.HumanRequired = true // content changes even though the version is constant
	tool.Risk = TierCritical
	tightened.Tools["delete"] = tool
	changed, err := NewGate(st, reviewerFunc(allowReview), exec, Config{Policy: tightened,
		StateReader: staticStateReader{}, Mode: Enforce})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.Dispatch(ctx, hash, p, "changed-policy"); !errors.Is(err, ErrStale) {
		t.Fatalf("changed policy content dispatched: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("changed policy reached executor")
	}
	var requestDigest, approvalDigest string
	if err := st.pool.QueryRow(ctx, `SELECT policy_digest FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&requestDigest); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT policy_digest FROM review_gate_approvals WHERE request_hash = $1`, hash).Scan(&approvalDigest); err != nil {
		t.Fatal(err)
	}
	if requestDigest != original.policyDigest || approvalDigest != original.policyDigest || requestDigest == changed.policyDigest {
		t.Fatalf("policy digests not bound: request=%s approval=%s changed=%s", requestDigest, approvalDigest, changed.policyDigest)
	}
}

func TestTrustedStateReaderRejectsChangedWorld(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		st, ctx := openTestStore(t)
		var world atomic.Value
		world.Store("world-1")
		reader := stateReaderFunc(func(context.Context, string, string) (string, error) {
			return world.Load().(string), nil
		})
		var calls atomic.Int32
		exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
			calls.Add(1)
			return ExecutionResult{Outcome: Confirmed}, nil
		})
		g, err := NewGate(st, reviewerFunc(allowReview), exec, Config{Policy: corePolicy(), StateReader: reader, Mode: Enforce})
		if err != nil {
			t.Fatal(err)
		}
		p := testProposal("write")
		p.StateFingerprint = "caller-controlled-old-state"
		hash := readyRequest(t, ctx, g, p)
		world.Store("world-2")
		// Reusing the approved caller fingerprint cannot hide the fresh world.
		p.StateFingerprint = "world-1"
		if _, err := g.Dispatch(ctx, hash, p, "changed-world"); !errors.Is(err, ErrStale) {
			t.Fatalf("changed world dispatched with old caller fingerprint: %v", err)
		}
		if calls.Load() != 0 {
			t.Fatal("changed world reached executor")
		}
	})
	t.Run("evaluate", func(t *testing.T) {
		st, ctx := openTestStore(t)
		world := "world-1"
		reader := stateReaderFunc(func(context.Context, string, string) (string, error) { return world, nil })
		g, err := NewGate(st, reviewerFunc(allowReview), executorFunc(confirmedExecute), Config{Policy: corePolicy(), StateReader: reader, Mode: Enforce})
		if err != nil {
			t.Fatal(err)
		}
		p := testProposal("write")
		d, err := g.Submit(ctx, "agent_1", p)
		if err != nil {
			t.Fatal(err)
		}
		world = "world-2"
		if _, err := g.Evaluate(ctx, d.RequestHash, "task"); !errors.Is(err, ErrStale) {
			t.Fatalf("changed world evaluated: %v", err)
		}
	})
}

func TestHeartbeatKeepsActiveExecutorDispatched(t *testing.T) {
	st, ctx := openTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		close(started)
		<-release // deliberately blocked beyond the original lease
		return ExecutionResult{Outcome: Confirmed}, nil
	})
	g, err := NewGate(st, reviewerFunc(allowReview), exec, Config{Policy: corePolicy(),
		StateReader: staticStateReader{}, Mode: Enforce, Lease: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	type dispatchOutcome struct {
		result DispatchResult
		err    error
	}
	done := make(chan dispatchOutcome, 1)
	go func() {
		result, err := g.Dispatch(ctx, hash, p, "heartbeated")
		done <- dispatchOutcome{result, err}
	}()
	<-started
	var oldLease time.Time
	if err := st.pool.QueryRow(ctx, `SELECT lease_until FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&oldLease); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if outcome, err := g.Recover(ctx, hash); err != nil || outcome != Dispatched {
		t.Fatalf("active owner was recovered: %s, %v", outcome, err)
	}
	if d, _ := g.Inspect(ctx, hash); d.State != StateDispatched {
		t.Fatalf("active call wrongly settled to %s", d.State)
	}
	var newLease time.Time
	if err := st.pool.QueryRow(ctx, `SELECT lease_until FROM review_gate_requests WHERE request_hash = $1`, hash).Scan(&newLease); err != nil || !newLease.After(oldLease) {
		t.Fatalf("heartbeat did not extend lease: old=%s new=%s err=%v", oldLease, newLease, err)
	}
	if hashes, err := g.ListRecovery(ctx, 10); err != nil || len(hashes) != 0 {
		t.Fatalf("active owner listed for recovery: %v, %v", hashes, err)
	}
	close(release)
	out := <-done
	if out.err != nil || out.result.Outcome != Confirmed {
		t.Fatalf("heartbeated dispatch: %+v, %v", out.result, out.err)
	}
}

func TestHumanRejectIsDurableAndTripsCircuit(t *testing.T) {
	st, ctx := openTestStore(t)
	g, err := NewGate(st, reviewerFunc(allowReview), executorFunc(confirmedExecute), Config{Policy: corePolicy(),
		StateReader: staticStateReader{}, Mode: Enforce, DenialThreshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("delete")
	p.SessionID = "reject-session"
	hash := awaitingRequest(t, ctx, g, p)
	if _, err := g.Reject(ctx, Rejection{RequestHash: hash, Actor: "agent_1", Reason: "self"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("proposer rejected own request: %v", err)
	}
	d, err := g.Reject(ctx, Rejection{RequestHash: hash, Actor: "operator_1", Reason: "unsafe"})
	if err != nil || d.State != StateDenied || !d.Risk.HumanRequired {
		t.Fatalf("human rejection: %+v, %v", d, err)
	}
	persisted, _ := g.Inspect(ctx, hash)
	if persisted.State != StateDenied || !persisted.Risk.HumanRequired {
		t.Fatalf("reconstructed rejection: %+v", persisted)
	}
	entries, _ := st.Audit(ctx)
	found := false
	for _, entry := range entries {
		if entry.RequestHash == hash && entry.Event == "HUMAN_REJECTED" && entry.Actor == "operator_1" {
			found = true
		}
	}
	if !found {
		t.Fatal("human rejection missing from audit")
	}
	other := testProposal("write")
	other.SessionID = p.SessionID
	if _, err := g.Submit(ctx, "agent_1", other); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("human rejection did not trip circuit: %v", err)
	}
}

func TestSQLApprovalPresenceAndPolicyGuard(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	p := testProposal("delete")
	hash := awaitingRequest(t, ctx, g, p)
	if _, err := st.pool.Exec(ctx, `UPDATE review_gate_requests SET state = 'APPROVED' WHERE request_hash = $1`, hash); err == nil {
		t.Fatal("SQL guard approved a request without an approval row")
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO review_gate_approvals
		(request_hash, operator_id, proposer_id, policy_digest, expires_at)
		VALUES ($1, 'operator_1', 'agent_1', $2, now() + interval '1 hour')`, hash, strings.Repeat("0", 64)); err == nil {
		t.Fatal("SQL guard accepted an approval for different policy content")
	}
	d, _ := g.Inspect(ctx, hash)
	if d.State != StateAwaitingHuman {
		t.Fatalf("rejected SQL bypass changed state to %s", d.State)
	}
}

func TestDispatchFinalStateCheckAfterBlockedRowLock(t *testing.T) {
	st, ctx := openTestStore(t)
	var world atomic.Value
	world.Store("world-1")
	var dispatchReads atomic.Int32
	var armed atomic.Bool
	firstRead := make(chan struct{})
	reader := stateReaderFunc(func(context.Context, string, string) (string, error) {
		if dispatchReads.Add(1) == 1 && armed.Load() {
			close(firstRead)
		}
		return world.Load().(string), nil
	})
	var executorCalls atomic.Int32
	exec := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		executorCalls.Add(1)
		return ExecutionResult{Outcome: Confirmed}, nil
	})
	g, err := NewGate(st, reviewerFunc(allowReview), exec, Config{Policy: corePolicy(), StateReader: reader, Mode: Enforce})
	if err != nil {
		t.Fatal(err)
	}
	p := testProposal("write")
	hash := readyRequest(t, ctx, g, p)
	dispatchReads.Store(0)
	armed.Store(true)

	// Hold the exact request row so Dispatch's first v1 read completes, then its
	// FOR UPDATE critical section blocks. Change the world to v2 before release.
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	lockTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(context.Background()) //nolint:errcheck
	if _, err := lockTx.Exec(ctx, `SELECT request_hash FROM review_gate_requests WHERE request_hash = $1 FOR UPDATE`, hash); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result DispatchResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := g.Dispatch(ctx, hash, p, "blocked-row-state-race")
		done <- outcome{result, err}
	}()
	<-firstRead
	blocked := false
	for i := 0; i < 100; i++ {
		var waiting int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%review_gate_requests%' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("dispatch never blocked on the request row lock")
	}
	world.Store("world-2")
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not finish after row lock release")
	}
	if !errors.Is(got.err, ErrStale) || got.result.Outcome != FailedDefinite {
		t.Fatalf("post-lock stale result: %+v, %v", got.result, got.err)
	}
	if executorCalls.Load() != 0 || dispatchReads.Load() != 2 {
		t.Fatalf("executor calls=%d state reads=%d, want 0 and 2", executorCalls.Load(), dispatchReads.Load())
	}
	d, err := g.Inspect(ctx, hash)
	if err != nil || d.State != StateFailedDefinite {
		t.Fatalf("stale no-execution state: %+v, %v", d, err)
	}
	var audited int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM review_gate_audit WHERE request_hash = $1 AND event = 'STALE_NO_EXECUTION'`, hash).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("stale no-execution audit rows=%d err=%v", audited, err)
	}
}
