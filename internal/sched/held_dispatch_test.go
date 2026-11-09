package sched

import (
	"context"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// While a mission is held, ten ticks over its ADMITTED state do no work and
// start nothing (no hot loop, no dispatch); after the release the mission is
// dispatched on the next tick (no lost wake).
func TestHeldNoHotLoopNoLostWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, ln, cfg := ownerSetup(t, "m_nl")
	s := New(st, cfg, ln)
	if _, err := s.consumeOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	plantAt(t, st, "m_nl", 1000, "ATTEMPT_STARTED", badType) // the consumer holds the mission on it
	for i := 0; i < 10; i++ {
		worked, err := s.Tick(ctx)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if worked {
			t.Fatalf("tick %d reported work over a held mission (a hot loop)", i)
		}
	}
	if n := countWhere(t, st, `SELECT count(*) FROM attempts WHERE mission_id = 'm_nl'`); n != 0 {
		t.Fatalf("a held mission was dispatched: %d attempts", n)
	}
	if !hasHold(t, st, "m_nl") {
		t.Fatal("the poisoned mission is not held")
	}
	unplant(t, st, "m_nl", 1000)
	for i := 0; i < 2 && countWhere(t, st, `SELECT count(*) FROM attempts WHERE mission_id = 'm_nl'`) == 0; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if hasHold(t, st, "m_nl") {
		t.Fatal("the hold was not released once the row decodes")
	}
	if n := countWhere(t, st, `SELECT count(*) FROM attempts WHERE mission_id = 'm_nl'`); n != 1 {
		t.Fatalf("after the release: %d attempts; the mission was not dispatched within a tick or two", n)
	}
	close(ln.block) // let the fake worker end
}

// unplant removes a planted row, as an operator repairing poisoned data would
// (the append-only triggers are lifted for that transaction only).
func unplant(t *testing.T, st *store.Store, m domain.MissionID, seq int) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{`ALTER TABLE events DISABLE TRIGGER events_append_only`, `ALTER TABLE outbox DISABLE TRIGGER USER`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`DELETE FROM events WHERE mission_id = $1 AND seq = $2`, `DELETE FROM outbox WHERE mission_id = $1 AND seq = $2`} {
		if _, err := tx.Exec(ctx, q, string(m), seq); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`ALTER TABLE events ENABLE TRIGGER events_append_only`, `ALTER TABLE outbox ENABLE TRIGGER USER`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
