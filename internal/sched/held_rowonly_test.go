package sched

import (
	"context"
	"fmt"
	"testing"

	"fabric/internal/domain"
)

// A poison in only one table: the view is built from the events row, so a bad
// events row holds the mission even when its outbox payload is intact, and a
// bad outbox payload alone (events row fine) does not.
func TestHeldPoisonInOneTableOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		evBad, obBad  bool
		wantHold, app bool
	}{
		{"events-row-only", true, false, true, false},
		{"outbox-payload-only", false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, s, _, b := heldSetup(t)
			var seq int64
			if err := st.Pool().QueryRow(ctx, `SELECT COALESCE(max(seq), 0) + 1 FROM events WHERE mission_id = $1`, string(b)).Scan(&seq); err != nil {
				t.Fatal(err)
			}
			good := fmt.Sprintf(waitEvent, string(b), seq)
			bad := fmt.Sprintf(badType, string(b), seq)
			evPayload, obPayload := good, good
			if tc.evBad {
				evPayload = bad
			}
			if tc.obBad {
				obPayload = bad
			}
			kind := "ATTEMPT_FINISHED"
			if tc.evBad {
				kind = "ATTEMPT_STARTED"
			}
			tx, err := st.Pool().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ($1, $2, $3, 'test', 'one-table', $4::jsonb)`, string(b), seq, kind, evPayload); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, string(b), seq, kind, obPayload); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.consumeOutbox(ctx); err != nil {
				t.Fatalf("consume: %v", err)
			}
			if got := hasHold(t, st, domain.MissionID(b)); got != tc.wantHold {
				t.Fatalf("held=%v, want %v", got, tc.wantHold)
			}
			if got := hasWait(t, st, domain.MissionID(b)); got != tc.app {
				t.Fatalf("wait row=%v, want %v", got, tc.app)
			}
		})
	}
}
