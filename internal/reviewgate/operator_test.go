package reviewgate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOperatorMetadataAndReasonsDoNotExposeSecrets(t *testing.T) {
	st, ctx := openTestStore(t)
	g := newTestGate(t, st, Enforce, reviewerFunc(allowReview), executorFunc(confirmedExecute))
	hash := awaitingRequest(t, ctx, g, testProposal("delete"))
	op, err := NewOperator(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Approve(ctx, Approval{RequestHash: hash, OperatorID: "agent_1", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("self approval accepted: %v", err)
	}
	summary, err := op.Inspect(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := op.List(ctx, StateAwaitingHuman, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("operator list: %+v %v", listed, err)
	}
	metadata, _ := json.Marshal([]any{summary, listed})
	if strings.Contains(string(metadata), "top-secret-payload") || strings.Contains(string(metadata), "canonical_proposal") {
		t.Fatalf("operator metadata exposed proposal data: %s", metadata)
	}
	const reason = "top-secret-operator-reason"
	if _, err := op.Approve(ctx, Approval{RequestHash: hash, OperatorID: "op:alice", Reason: reason, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	audit, err := st.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(audit)
	if strings.Contains(string(encoded), reason) || !strings.Contains(string(encoded), "reason_sha256=") {
		t.Fatalf("operator reason was not digest-only: %s", encoded)
	}
}
