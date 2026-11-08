package main

import (
	"testing"
	"time"
)

func TestReviewOperatorActor(t *testing.T) {
	for _, actor := range []string{"", "chat", "op:", "op:two people", "agent:one"} {
		t.Setenv("FABRIC_ACTOR", actor)
		if _, err := reviewOperatorActor(); err == nil {
			t.Errorf("actor %q accepted", actor)
		}
	}
	t.Setenv("FABRIC_ACTOR", "op:alice")
	if actor, err := reviewOperatorActor(); err != nil || actor != "op:alice" {
		t.Fatalf("valid actor: %q %v", actor, err)
	}
}

func TestReviewApprovalExpiryAndReason(t *testing.T) {
	now := time.Now()
	for _, value := range []string{"", "0s", "-1m", "25h", "forever"} {
		if _, err := reviewApprovalExpiry(value, now); err == nil {
			t.Errorf("expiry %q accepted", value)
		}
	}
	if got, err := reviewApprovalExpiry("30m", now); err != nil || !got.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("valid expiry: %v %v", got, err)
	}
	if err := validateReviewReason(""); err == nil {
		t.Fatal("empty rejection/approval reason accepted")
	}
	if err := validateReviewReason("reviewed exact request"); err != nil {
		t.Fatal(err)
	}
}
