package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"fabric/internal/reviewgate"
	"fabric/internal/storetest"
)

type cliReviewState struct{}

func (cliReviewState) CurrentFingerprint(context.Context, string, string) (string, error) {
	return "cli-review-state", nil
}

type cliReviewExecutor struct{}

func (cliReviewExecutor) Execute(context.Context, reviewgate.DispatchRequest) (reviewgate.ExecutionResult, error) {
	return reviewgate.ExecutionResult{Outcome: reviewgate.Confirmed}, nil
}

func TestReviewApproveCLIWakesDurableWaiter(t *testing.T) {
	st := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tx, err := st.Pool().Begin(ctx)
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
	reviewStore := reviewgate.NewPostgresStore(st.Pool())
	gate, err := reviewgate.NewGate(reviewStore, reviewgate.LocalPolicyReviewer{Policy: reviewgate.Policy{
		Version: reviewgate.PolicyV1, ModelMaxTier: reviewgate.TierLow,
		Tools: map[string]reviewgate.ToolPolicy{"shell": {Allow: true, Risk: reviewgate.TierHigh, Reversibility: reviewgate.Irreversible}},
	}}, cliReviewExecutor{}, reviewgate.Config{Policy: reviewgate.Policy{
		Version: reviewgate.PolicyV1, ModelMaxTier: reviewgate.TierLow,
		Tools: map[string]reviewgate.ToolPolicy{"shell": {Allow: true, Risk: reviewgate.TierHigh, Reversibility: reviewgate.Irreversible}},
	}, Mode: reviewgate.Enforce, StateReader: cliReviewState{}})
	if err != nil {
		t.Fatal(err)
	}
	proposal := reviewgate.Proposal{MissionID: "m_cli_review", SessionID: "a_cli_review", Tool: "shell",
		Args: json.RawMessage(`{"command":"deploy"}`), StateFingerprint: "placeholder",
		Provenance: []reviewgate.ProvenanceHandle{{ID: "prompt", Origin: reviewgate.OriginUser}, {ID: "call", Origin: reviewgate.OriginAgent, DerivedFrom: []string{"prompt"}}}}
	decision, err := gate.Submit(ctx, "agent:cli_review", proposal)
	if err != nil {
		t.Fatal(err)
	}
	decision, err = gate.Evaluate(ctx, decision.RequestHash, "deploy after review")
	if err != nil || decision.State != reviewgate.StateAwaitingHuman {
		t.Fatalf("CLI fixture did not await review: %+v %v", decision, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- reviewStore.WaitForReviewDecision(ctx, decision.RequestHash) }()
	t.Setenv("FABRIC_DSN", storetest.DSN)
	t.Setenv("FABRIC_ACTOR", "op:cli-integration")
	if err := reviewMain([]string{"approve", "--hash", decision.RequestHash, "--reason", "exact CLI request reviewed", "--expires-in", "1h"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI approval did not wake the durable review waiter")
	}
}
