package reviewgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/agent"
	"fabric/internal/domain"
	fabricstore "fabric/internal/store"
	"fabric/internal/storetest"
)

type runnerReviewFixture struct {
	ctx         context.Context
	owner       *fabricstore.Store
	ownerReview *PostgresStore
	appReview   *PostgresStore
	runnerPool  *pgxpool.Pool
	proposal    Proposal
}

func newRunnerReviewFixture(t *testing.T) runnerReviewFixture {
	t.Helper()
	ownerReview, ctx := openTestStore(t)
	owner, err := fabricstore.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := owner.Truncate(ctx); err != nil {
		t.Fatal(err)
	}
	oldDSN := storetest.DSN
	storetest.DSN = testDSN
	t.Cleanup(func() { storetest.DSN = oldDSN })
	appDSN, runnerDSN := storetest.AppRunnerDSNs(t, owner)
	appReview, err := Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appReview.Close)
	runnerStore, err := fabricstore.Open(ctx, runnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runnerStore.Close)
	if err := runnerStore.CheckRunnerRole(ctx); err != nil {
		t.Fatal(err)
	}

	p := testProposal("write")
	p.SessionID = "r_runner_review"
	p.Args = json.RawMessage(`{"path":"/tmp/runner","note":"safe"}`)
	p.Provenance = append(p.Provenance, ProvenanceHandle{ID: "call-0-toolu_runner", Origin: OriginAgent, DerivedFrom: []string{"h_user"}})
	for _, q := range []string{
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_1', 'runner-review', '{}'::jsonb, 'anthropic-api')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role, path, lock_path, lease_until)
		 VALUES ('r_runner_review', 'm_1', 1, 0, 0, 'RUNNING', 'h_test', 'worker', 'anthropic-api', '/nonexistent/fabric-rig/native-locks/r_runner_review.lock', now() + interval '1 hour')`,
		`INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha) VALUES ('r_runner_review', 0, 'assistant', '[]', 1, 'runner-fixture')`,
		`INSERT INTO agent_tool_calls (attempt_id, tool_use_id, msg_idx, name, input, side_effecting, fence)
		 VALUES ('r_runner_review', 'toolu_runner', 0, 'write', '{"path":"/tmp/runner","note":"safe"}', true, 1)`,
	} {
		if _, err := owner.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return runnerReviewFixture{ctx: ctx, owner: owner, ownerReview: ownerReview, appReview: appReview, runnerPool: runnerStore.Pool(), proposal: p}
}

func (f runnerReviewFixture) gate(t *testing.T, human bool, executor Executor) *Gate {
	t.Helper()
	policy := corePolicy()
	write := policy.Tools["write"]
	write.HumanRequired = human
	write.AllowTaintedSideEffects = !human
	policy.Tools["write"] = write
	g, err := NewGate(f.appReview, reviewerFunc(allowReview), executor, Config{Policy: policy, Mode: Enforce, StateReader: staticStateReader{}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func reviewSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func runnerQueryError(ctx context.Context, pool *pgxpool.Pool, query string) error {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func TestRunnerCannotExecuteTrustedReviewBrokersOrReadTables(t *testing.T) {
	f := newRunnerReviewFixture(t)
	rows, err := f.runnerPool.Query(f.ctx, `SELECT p.oid::regprocedure::text, p.pronargs
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.proname LIKE 'fabric\_review\_gate\_broker\_%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var functions []string
	for rows.Next() {
		var signature string
		var nargs int
		if err := rows.Scan(&signature, &nargs); err != nil {
			t.Fatal(err)
		}
		args := make([]string, nargs)
		for i := range args {
			args[i] = "NULL"
		}
		name := signature[:strings.Index(signature, "(")]
		functions = append(functions, fmt.Sprintf("SELECT * FROM %s(%s)", name, strings.Join(args, ",")))
	}
	rows.Close()
	if len(functions) != 19 {
		t.Fatalf("found %d trusted 0033 brokers, want 19", len(functions))
	}
	for _, query := range functions {
		err := runnerQueryError(f.ctx, f.runnerPool, query)
		if err == nil {
			t.Fatalf("runner executed trusted broker: %s", query)
		}
		if reviewSQLState(err) != "42501" {
			t.Fatalf("broker error = %v (SQLSTATE %s), want 42501", err, reviewSQLState(err))
		}
	}

	// Revocation is the first boundary; the in-function session_user guard is a
	// second boundary even if an administrator accidentally grants EXECUTE.
	if _, err := f.owner.Pool().Exec(f.ctx, `GRANT EXECUTE ON FUNCTION fabric_review_gate_broker_load(text,boolean) TO `+f.runnerPool.Config().ConnConfig.User); err != nil {
		t.Fatal(err)
	}
	err = runnerQueryError(f.ctx, f.runnerPool, `SELECT * FROM fabric_review_gate_broker_load(NULL,NULL)`)
	if err == nil {
		t.Fatal("defensive broker guard did not reject runner")
	}
	if reviewSQLState(err) != "42501" {
		t.Fatalf("defensive guard error = %v, want SQLSTATE 42501", err)
	}
	for _, table := range []string{"review_gate_requests", "review_gate_results"} {
		if _, err := f.runnerPool.Exec(f.ctx, `SELECT * FROM `+table+` WHERE false`); reviewSQLState(err) != "42501" {
			t.Fatalf("runner read %s: %v, want SQLSTATE 42501", table, err)
		}
	}
}

func TestAppReviewLifecycleAndRunnerUsesOnlyDurableResult(t *testing.T) {
	f := newRunnerReviewFixture(t)
	var calls atomic.Int32
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		calls.Add(1)
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "runner durable stdout", IsError: false}}, nil
	})
	g := f.gate(t, true, executor)
	d, err := g.Submit(f.ctx, "agent:r_runner_review", f.proposal)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- f.appReview.WaitForReviewDecision(f.ctx, d.RequestHash) }()
	d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only")
	if err != nil || d.State != StateAwaitingHuman {
		t.Fatalf("app evaluate: %+v %v", d, err)
	}
	operator, err := NewOperator(f.ownerReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operator.Approve(f.ctx, Approval{RequestHash: d.RequestHash, OperatorID: "op:runner", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app waiter missed committed approval")
	}
	if _, err := g.Dispatch(f.ctx, d.RequestHash, f.proposal, "runner-dispatch"); err != nil {
		t.Fatal(err)
	}
	stored, err := g.LoadToolResult(f.ctx, d.RequestHash)
	if err != nil || stored.Output != "runner durable stdout" || calls.Load() != 1 {
		t.Fatalf("app result: %+v calls=%d err=%v", stored, calls.Load(), err)
	}
	replay := f.gate(t, true, executor)
	if _, err := replay.Dispatch(f.ctx, d.RequestHash, f.proposal, "runner-dispatch"); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.LoadToolResult(f.ctx, d.RequestHash); err != nil || calls.Load() != 1 {
		t.Fatalf("app replay: calls=%d err=%v", calls.Load(), err)
	}

	db := &agent.DB{Pool: f.runnerPool}
	for _, tc := range []struct {
		name, attempt, call string
		fence               int64
	}{
		{"wrong attempt", "r_missing", "toolu_runner", 1},
		{"wrong fence", "r_runner_review", "toolu_runner", 2},
		{"wrong call", "r_runner_review", "toolu_missing", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.FinishReviewedTool(f.ctx, domain.AttemptID(tc.attempt), tc.fence, tc.call, []byte("forged"), true); err == nil {
				t.Fatal("invalid exact settlement succeeded")
			}
			assertPendingRunnerCall(t, f)
		})
	}
	for _, query := range []string{
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role, path, lock_path, lease_until)
		 VALUES ('r_other', 'm_1', 2, 0, 0, 'RUNNING', 'h_test', 'worker', 'anthropic-api', '/nonexistent/fabric-rig/native-locks/r_other.lock', now() + interval '1 hour')`,
		`INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha) VALUES ('r_other', 0, 'assistant', '[]', 2, 'cross-attempt-fixture')`,
		`INSERT INTO agent_tool_calls (attempt_id, tool_use_id, msg_idx, name, input, side_effecting, fence)
		 VALUES ('r_other', 'toolu_runner', 0, 'write', '{"path":"/tmp/runner","note":"safe"}', true, 2)`,
	} {
		if _, err := f.owner.Pool().Exec(f.ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishReviewedTool(f.ctx, "r_other", 2, "toolu_runner", []byte("cross attempt"), false); err == nil {
		t.Fatal("cross-attempt settlement succeeded")
	}
	assertPendingRunnerCall(t, f)
	if _, err := f.runnerPool.Exec(f.ctx, `UPDATE review_gate_results SET output = 'forged' WHERE request_hash = $1`, d.RequestHash); reviewSQLState(err) != "42501" {
		t.Fatalf("runner forged a durable result association: %v", err)
	}
	assertPendingRunnerCall(t, f)

	if err := db.FinishReviewedTool(f.ctx, "r_runner_review", 1, "toolu_runner", []byte("forged runner output"), true); err != nil {
		t.Fatalf("runner exact settlement: %v", err)
	}
	var output []byte
	var isError bool
	if err := f.owner.Pool().QueryRow(f.ctx, `SELECT result, is_error FROM agent_tool_calls
		WHERE attempt_id = 'r_runner_review' AND tool_use_id = 'toolu_runner'`).Scan(&output, &isError); err != nil {
		t.Fatal(err)
	}
	if string(output) != "runner durable stdout" || isError {
		t.Fatalf("settlement trusted caller bytes: output=%q is_error=%v", output, isError)
	}
}

func assertPendingRunnerCall(t *testing.T, f runnerReviewFixture) {
	t.Helper()
	var state string
	if err := f.owner.Pool().QueryRow(f.ctx, `SELECT state FROM agent_tool_calls
		WHERE attempt_id = 'r_runner_review' AND tool_use_id = 'toolu_runner'`).Scan(&state); err != nil || state != "PENDING" {
		t.Fatalf("failed settlement changed call: state=%s err=%v", state, err)
	}
}

func TestRunnerSettlementRejectsCorruptDurableResultDigest(t *testing.T) {
	f := newRunnerReviewFixture(t)
	executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
		return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "durable stdout"}}, nil
	})
	g := f.gate(t, false, executor)
	d, err := g.Submit(f.ctx, "agent:r_runner_review", f.proposal)
	if err != nil {
		t.Fatal(err)
	}
	d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Dispatch(f.ctx, d.RequestHash, f.proposal, "corrupt-durable-result"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Pool().Exec(f.ctx, `ALTER TABLE review_gate_results DISABLE TRIGGER review_gate_result_guard`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.owner.Pool().Exec(context.Background(), `ALTER TABLE review_gate_results ENABLE TRIGGER review_gate_result_guard`); err != nil {
			t.Errorf("re-enable durable result guard: %v", err)
		}
	})
	tag, err := f.owner.Pool().Exec(f.ctx, `UPDATE review_gate_results SET output = 'owner-corrupted-output' WHERE request_hash = $1`, d.RequestHash)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("owner corruption updated %d rows, want 1", tag.RowsAffected())
	}
	if _, err := f.owner.Pool().Exec(f.ctx, `ALTER TABLE review_gate_results ENABLE TRIGGER review_gate_result_guard`); err != nil {
		t.Fatal(err)
	}

	db := &agent.DB{Pool: f.runnerPool}
	err = db.FinishReviewedTool(f.ctx, "r_runner_review", 1, "toolu_runner", []byte("forged caller output"), true)
	if reviewSQLState(err) != "23514" {
		t.Fatalf("corrupt durable result settlement error = %v (SQLSTATE %s), want 23514", err, reviewSQLState(err))
	}
	assertPendingRunnerCall(t, f)
}

func TestRunnerFixedMetadataForLegacyAndControlOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
		deny    bool
		want    string
	}{
		{"confirmed without durable result", Confirmed, false, "confirmed_result_unavailable"},
		{"definite failure without durable result", FailedDefinite, false, "failed_definite"},
		{"unknown", Unknown, false, "unknown_outcome"},
		{"denied", Confirmed, true, "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunnerReviewFixture(t)
			executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
				return ExecutionResult{Outcome: tc.outcome}, nil
			})
			g := f.gate(t, false, executor)
			d, err := g.Submit(f.ctx, "agent:r_runner_review", f.proposal)
			if err != nil {
				t.Fatal(err)
			}
			d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only")
			if err != nil {
				t.Fatal(err)
			}
			if tc.deny {
				if _, err := g.Deny(f.ctx, d.RequestHash, "test_denial"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := g.Dispatch(f.ctx, d.RequestHash, f.proposal, "metadata-"+tc.name); tc.outcome == Unknown {
				if !errors.Is(err, ErrUnknown) {
					t.Fatalf("UNKNOWN dispatch error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			db := &agent.DB{Pool: f.runnerPool}
			if err := db.FinishReviewedTool(f.ctx, "r_runner_review", 1, "toolu_runner", []byte("forged"), false); err != nil {
				t.Fatal(err)
			}
			var output string
			var isError bool
			if err := f.owner.Pool().QueryRow(f.ctx, `SELECT convert_from(result,'UTF8'), is_error FROM agent_tool_calls
				WHERE attempt_id = 'r_runner_review' AND tool_use_id = 'toolu_runner'`).Scan(&output, &isError); err != nil {
				t.Fatal(err)
			}
			var control ToolGateResult
			if err := json.Unmarshal([]byte(output), &control); err != nil || control.Status != tc.want || control.RequestHash != d.RequestHash || !isError {
				t.Fatalf("fixed control outcome = %s is_error=%v err=%v, want %s", output, isError, err, tc.want)
			}
		})
	}
}

func TestApprovedShadowAndEvaluateSettleWithoutExecution(t *testing.T) {
	for _, mode := range []RolloutMode{Shadow, Evaluate} {
		t.Run(string(mode), func(t *testing.T) {
			f := newRunnerReviewFixture(t)
			var calls atomic.Int32
			executor := executorFunc(func(context.Context, DispatchRequest) (ExecutionResult, error) {
				calls.Add(1)
				return ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: "must not execute"}}, nil
			})
			g := f.gate(t, true, executor)
			policy := corePolicy()
			write := policy.Tools["write"]
			write.HumanRequired = true
			policy.Tools["write"] = write
			var err error
			g, err = NewGate(f.appReview, reviewerFunc(allowReview), executor, Config{Policy: policy, Mode: mode, StateReader: staticStateReader{}})
			if err != nil {
				t.Fatal(err)
			}
			d, err := g.Submit(f.ctx, "agent:r_runner_review", f.proposal)
			if err != nil {
				t.Fatal(err)
			}
			d, err = g.Evaluate(f.ctx, d.RequestHash, "original task only")
			if err != nil || d.State != StateAwaitingHuman {
				t.Fatalf("evaluate: %+v %v", d, err)
			}
			operator, err := NewOperator(f.ownerReview)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := operator.Approve(f.ctx, Approval{RequestHash: d.RequestHash, OperatorID: "op:no-exec", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Dispatch(f.ctx, d.RequestHash, f.proposal, string(mode)+"-no-exec"); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.owner.Pool().QueryRow(f.ctx, `SELECT state FROM review_gate_requests WHERE request_hash = $1`, d.RequestHash).Scan(&state); err != nil || state != "READY" {
				t.Fatalf("approved no-execution state=%s err=%v", state, err)
			}
			db := &agent.DB{Pool: f.runnerPool}
			if err := db.FinishReviewedTool(f.ctx, "r_runner_review", 1, "toolu_runner", []byte("forged"), false); err != nil {
				t.Fatal(err)
			}
			var output string
			if err := f.owner.Pool().QueryRow(f.ctx, `SELECT convert_from(result,'UTF8') FROM agent_tool_calls
				WHERE attempt_id = 'r_runner_review' AND tool_use_id = 'toolu_runner'`).Scan(&output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, string(mode)+"_no_execution") || calls.Load() != 0 {
				t.Fatalf("no-execution settlement output=%s calls=%d", output, calls.Load())
			}
		})
	}
}
