package reviewgate

import (
	"context"
	"testing"
)

func TestLocalPolicyReviewerAndExplicitSafeSideEffect(t *testing.T) {
	p := coreProposal()
	p.Provenance = []ProvenanceHandle{{ID: "call", Origin: OriginAgent}}
	risk, err := Classify(p, corePolicy())
	if err != nil || !risk.HumanRequired {
		t.Fatalf("agent-derived side effect did not default to human: %+v %v", risk, err)
	}
	policy := corePolicy()
	tool := policy.Tools["write"]
	tool.HumanRequired = false
	tool.AllowTaintedSideEffects = true
	policy.Tools["write"] = tool
	risk, err = Classify(p, policy)
	if err != nil || risk.HumanRequired || !risk.Tainted {
		t.Fatalf("explicit safe side effect was not permitted: %+v %v", risk, err)
	}
	canonical, err := CanonicalProposal(p)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := (LocalPolicyReviewer{Policy: policy}).Review(context.Background(), ReviewInput{
		OriginalTask: "original user prompt", CanonicalProposal: canonical, Risk: risk,
	})
	if err != nil || decision.Verdict != ReviewAllow {
		t.Fatalf("local policy reviewer: %+v %v", decision, err)
	}
	risk.PolicyDigest = "caller-forged-digest"
	if _, err := (LocalPolicyReviewer{Policy: policy}).Review(context.Background(), ReviewInput{
		OriginalTask: "original user prompt", CanonicalProposal: canonical, Risk: risk,
	}); err == nil {
		t.Fatal("local reviewer trusted caller risk metadata")
	}
}
