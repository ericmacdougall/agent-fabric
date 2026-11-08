package reviewgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

// LocalPolicyReviewer is the deterministic production reviewer. It recomputes
// policy classification from the isolated ReviewInput; it has no model API,
// transcript, executor, approval store, or other capability.
type LocalPolicyReviewer struct {
	Policy Policy
}

func (r LocalPolicyReviewer) Review(_ context.Context, in ReviewInput) (ModelDecision, error) {
	if !validText(in.OriginalTask, MaxTaskBytes) {
		return ModelDecision{}, invalid("invalid original task")
	}
	var p Proposal
	if err := json.Unmarshal(in.CanonicalProposal, &p); err != nil {
		return ModelDecision{}, invalid("review proposal is not JSON")
	}
	canonical, err := CanonicalProposal(p)
	if err != nil || !bytes.Equal(canonical, in.CanonicalProposal) {
		return ModelDecision{}, invalid("review proposal is not canonical")
	}
	risk, err := Classify(p, r.Policy)
	if err != nil {
		return ModelDecision{}, err
	}
	if !reflect.DeepEqual(risk, in.Risk) {
		return ModelDecision{}, fmt.Errorf("%w: reviewer risk input differs from policy", ErrConflict)
	}
	return ModelDecision{Verdict: ReviewAllow, Reason: "deterministic policy checks passed"}, nil
}
