package sched

import (
	"context"
	"errors"
	"testing"
	"time"

	"fabric/internal/store"
)

// Review M2 on PR 32: when the abandon after an undecodable snapshot fails,
// the mission must not be held. A hold would exempt its live attempt from
// lease expiry and reconcile and keep its slot (and the deploy busy gate)
// forever; without it the lease expires and the next start abandons cleanly.
func TestNoHoldWhenAbandonFails(t *testing.T) {
	ctx := context.Background()
	st, ln, cfg := ownerSetup(t, "m_noab")
	s := New(st, cfg, ln)
	s.settleBackoff = time.Millisecond
	if _, err := s.consumeOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	s.afterRead = func() { plantAt(t, st, "m_noab", 1000, "ATTEMPT_STARTED", badType) }
	s.afterFinishExec = func(resp store.Response, err error) error {
		return errors.New("test: the abandon cannot be committed")
	}
	if err := s.runOnce(ctx, "m_noab"); err == nil {
		t.Fatal("runOnce succeeded although the abandon failed")
	}
	if hasHold(t, st, "m_noab") {
		t.Fatal("the mission was held although its abandon failed")
	}
}
