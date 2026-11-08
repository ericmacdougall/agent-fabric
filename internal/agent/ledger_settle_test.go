package agent_test

import (
	"context"
	"testing"
	"time"

	"fabric/internal/agent"
)

func TestSettleLocksReservationResourcesNotCurrentCaps(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	oldResource := agent.SpendResource("month", "202601")
	newResource := agent.SpendResource("month", "202602")
	led := &agent.Ledger{St: r.st, Attempt: attemptID, Fence: fence, TTL: time.Hour,
		Caps: []agent.Cap{{Resource: oldResource, Total: 1000}}}
	rs, err := led.Hold(ctx, "settle-old-month", 10)
	if err != nil {
		t.Fatal(err)
	}
	led.Caps[0].Resource = newResource

	locker, err := r.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Release()
	if _, err := locker.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 7))`, oldResource); err != nil {
		t.Fatal(err)
	}
	settleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- led.Settle(settleCtx, rs, 10) }()
	select {
	case err := <-done:
		t.Errorf("Settle did not wait for the reservation resource lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := locker.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 7))`, oldResource); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-settleCtx.Done():
		t.Fatal("Settle remained blocked after the old resource was unlocked")
	}
}
