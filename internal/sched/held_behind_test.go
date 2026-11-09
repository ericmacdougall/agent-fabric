package sched

import (
	"context"
	"testing"
)

// A poison row behind the scheduler cursor (an older binary consumed past it
// without holding the mission) leaves nothing to drain, so the hold a start
// writes must stay until the log decodes: releasing it at once would restart
// the mission every tick.
func TestHoldBehindCursorStaysHeld(t *testing.T) {
	ctx := context.Background()
	st, ln, cfg := ownerSetup(t, "m_behind")
	s := New(st, cfg, ln)
	plantAt(t, st, "m_behind", 1000, "ATTEMPT_STARTED", badType)
	// The old binary's consumer moved the cursor past the poison.
	if _, err := st.Pool().Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ('sched', (SELECT max(id) FROM outbox))
		ON CONFLICT (consumer) DO UPDATE SET last_id = EXCLUDED.last_id`); err != nil {
		t.Fatal(err)
	}
	if err := s.runOnce(ctx, "m_behind"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !hasHold(t, st, "m_behind") {
		t.Fatal("the failed snapshot did not hold the mission")
	}
	for i := 0; i < 3; i++ {
		if _, err := s.consumeOutbox(ctx); err != nil {
			t.Fatal(err)
		}
		if !hasHold(t, st, "m_behind") {
			t.Fatalf("drain %d released a hold although the log still does not decode", i)
		}
	}
}
