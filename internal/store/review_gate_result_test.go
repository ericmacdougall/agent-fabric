package store

import (
	"context"
	"testing"
)

func TestReviewGateResultSchemaBoundsAndTerminalBinding(t *testing.T) {
	st, ctx := openTest(t)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SET LOCAL fabric.allow_truncate = 'on'",
		"TRUNCATE review_gate_approvals, review_gate_audit, review_gate_requests, review_gate_circuits RESTART IDENTITY CASCADE",
		"UPDATE review_gate_audit_head SET seq = 0, last_hash = '' WHERE id = 1",
	} {
		if _, err := tx.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := st.pool.Exec(ctx, `INSERT INTO review_gate_requests
		(request_hash, mission_id, session_id, tool, proposer_id, policy_version, policy_digest, mode, canonical_proposal, state)
		VALUES ($1, 'm_result', 'a_result', 'shell', 'agent:result', 'review-gate/v1',
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'enforce',
			'{"args":{},"mission_id":"m_result","provenance":[],"session_id":"a_result","state_fingerprint":"state","tool":"shell"}', 'PENDING')`, hash); err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, ctx,
		`UPDATE review_gate_requests SET state = 'READY', risk_tier = 1, reversibility = 'reversible', tainted = false, human_required = false`,
		`UPDATE review_gate_requests SET state = 'DISPATCHED', dispatch_idem_key = 'result-key', dispatch_fence = 1, lease_until = now() + interval '1 hour'`,
		`UPDATE review_gate_requests SET state = 'CONFIRMED', lease_until = NULL`)
	if _, err := st.pool.Exec(ctx, `INSERT INTO review_gate_results
		(request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format, output, is_error, truncated, output_sha256)
		VALUES ($1, 1, 'result-key', 'CONFIRMED', 'redacted-bounded-v1', convert_to(repeat('x', 65537), 'UTF8'), false, true,
			'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc')`, hash); err == nil {
		t.Fatal("schema accepted a durable result above 64 KiB")
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO review_gate_results
		(request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format, output, is_error, truncated, output_sha256)
		VALUES ($1, 2, 'result-key', 'CONFIRMED', 'redacted-bounded-v1', ''::bytea, false, false,
			'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc')`, hash); err == nil {
		t.Fatal("schema accepted a result for the wrong dispatch fence")
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO review_gate_results
		(request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format, output, is_error, truncated, output_sha256)
		VALUES ($1, 1, 'result-key', 'CONFIRMED', 'redacted-bounded-v1', 'redacted', false, false,
			'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc')`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE review_gate_results SET is_error = true WHERE request_hash = $1`, hash); err == nil {
		t.Fatal("schema allowed a durable result to be rewritten")
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM fabric_schema WHERE name = 'migrations/0034_review_gate_resume.sql'`); n != 1 {
		t.Fatalf("0034 migration count = %d", n)
	}
}

func TestReviewGateResultCleanupUsesTestOnlyTruncateGuard(t *testing.T) {
	st, ctx := openTest(t)
	// The shared fixtures predate review tables. This proves their explicit
	// cleanup also covers the new result table through the guarded FK cascade.
	tx, err := st.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL fabric.allow_truncate = 'on'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "TRUNCATE review_gate_requests CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
