package sched

import (
	"context"
	"fmt"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// These tests use only SQL and consumeOutbox, so they compile and fail
// against the code before F1a (a poisoned row stops the consumer there).

func heldSetup(t *testing.T) (*store.Store, *Scheduler, domain.MissionID, domain.MissionID) {
	t.Helper()
	st, ln, cfg := ownerSetup(t, "m_held_a")
	ctx := context.Background()
	if err := st.CreateMission(ctx, "m_held_b", domain.ProviderClaudeCode, "GoodQA goodqa#15372", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_held_b", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_held_b"}); err != nil {
		t.Fatal(err)
	}
	s := New(st, cfg, ln)
	if _, err := s.consumeOutbox(ctx); err != nil { // catch up on the admissions
		t.Fatal(err)
	}
	return st, s, "m_held_a", "m_held_b"
}

// plant appends one event to a mission's log and outbox, as a writer would.
func plant(t *testing.T, st *store.Store, mission domain.MissionID, kind, payloadFmt string) int64 {
	t.Helper()
	ctx := context.Background()
	var seq int64
	if err := st.Pool().QueryRow(ctx, `SELECT COALESCE(max(seq), 0) + 1 FROM events WHERE mission_id = $1`, string(mission)).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(payloadFmt, string(mission), seq)
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ($1, $2, $3, 'test', $4, $5::jsonb)`,
		string(mission), seq, kind, fmt.Sprintf("plant-%s-%d", mission, seq), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, string(mission), seq, kind, payload); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return seq
}

const (
	waitEvent    = `{"kind":"ATTEMPT_FINISHED","mission":%q,"seq":%d,"outcome":"wait","wait_key":"event:x"}`
	resumedEvent = `{"kind":"MISSION_RESUMED","mission":%q,"seq":%d}`
	badType      = `{"kind":"ATTEMPT_STARTED","mission":%q,"seq":%d,"fence":"not-a-number"}`
	unknownKind  = `{"kind":"NEW_FUTURE_KIND","mission":%q,"seq":%d}`
)

func countWhere(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func hasHold(t *testing.T, st *store.Store, m domain.MissionID) bool {
	return countWhere(t, st, `SELECT count(*) FROM outbox_cursors WHERE consumer = $1`, "sched.hold."+string(m)) == 1
}

func hasWait(t *testing.T, st *store.Store, m domain.MissionID) bool {
	return countWhere(t, st, `SELECT count(*) FROM waits WHERE mission_id = $1`, string(m)) == 1
}

func counter(t *testing.T, st *store.Store) int {
	return countWhere(t, st, `SELECT COALESCE((SELECT value FROM sched_counters WHERE name = 'waits_resumed_total'), 0)`)
}

// A mission with a stored event this build cannot decode is held; the other
// mission keeps flowing; the held mission's later rows are held in order and
// delivered once after the row decodes; a healthy row is delivered once.
func TestHeldMission(t *testing.T) {
	for _, tc := range []struct{ name, kind, bad string }{
		{"type-mismatch", "ATTEMPT_STARTED", badType},
		{"unknown-kind", "NEW_FUTURE_KIND", unknownKind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, s, a, b := heldSetup(t)
			plant(t, st, b, tc.kind, tc.bad)
			plant(t, st, a, "ATTEMPT_FINISHED", waitEvent)
			plant(t, st, b, "ATTEMPT_FINISHED", waitEvent) // behind the poison: held, in order
			plant(t, st, a, "MISSION_RESUMED", resumedEvent)

			n, err := s.consumeOutbox(ctx)
			if err != nil {
				t.Fatalf("a poisoned row stopped the consumer: %v", err)
			}
			if n != 2 {
				t.Fatalf("applied %d rows, want 2 (the healthy mission's)", n)
			}
			if !hasHold(t, st, b) || hasHold(t, st, a) {
				t.Fatal("expected exactly the poisoned mission to be held")
			}
			if hasWait(t, st, b) {
				t.Fatal("a row behind the poison was applied while the mission is held")
			}
			if counter(t, st) != 1 {
				t.Fatalf("healthy mission's resume counted %d times, want 1", counter(t, st))
			}
			// No hot loop: only held rows is idle.
			if n, err := s.consumeOutbox(ctx); err != nil || n != 0 {
				t.Fatalf("second tick over only held rows: n=%d err=%v", n, err)
			}

			// A fixed decoder: the row now decodes (the stored bytes are repaired
			// by the owner, as a fix would be); the hold drains in order and releases.
			fix := fmt.Sprintf(`{"kind":"ATTEMPT_STARTED","mission":%q,"seq":2}`, string(b))
			if tc.kind == "NEW_FUTURE_KIND" {
				fix = fmt.Sprintf(`{"kind":"INTENT_REVISED","mission":%q,"seq":2}`, string(b))
			}
			repair(t, st, b, 2, fix, tc.kind)
			if n, err := s.consumeOutbox(ctx); err != nil || n != 2 {
				t.Fatalf("drain applied %d rows (err %v), want 2", n, err)
			}
			if hasHold(t, st, b) {
				t.Fatal("the hold was not released after the drain caught up")
			}
			if !hasWait(t, st, b) {
				t.Fatal("the held mission's later row was never delivered")
			}
			if n, err := s.consumeOutbox(ctx); err != nil || n != 0 || counter(t, st) != 1 {
				t.Fatalf("redelivery after release: n=%d err=%v counter=%d", n, err, counter(t, st))
			}
		})
	}
}

// repair rewrites one stored row, as a fixed decoder's author would repair
// poisoned data, with the append-only trigger lifted for that statement.
func repair(t *testing.T, st *store.Store, m domain.MissionID, seq int, payload, oldKind string) {
	t.Helper()
	ctx := context.Background()
	newKind := "ATTEMPT_STARTED"
	if oldKind == "NEW_FUTURE_KIND" {
		newKind = "INTENT_REVISED"
	}
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{
		`ALTER TABLE events DISABLE TRIGGER events_append_only`,
		`ALTER TABLE outbox DISABLE TRIGGER USER`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE events SET kind = $3, payload = $4::jsonb WHERE mission_id = $1 AND seq = $2`, string(m), seq, newKind, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET kind = $3, payload = $4::jsonb WHERE mission_id = $1 AND seq = $2`, string(m), seq, newKind, payload); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE events ENABLE TRIGGER events_append_only`,
		`ALTER TABLE outbox ENABLE TRIGGER USER`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// A hold survives a restart: a second scheduler on the same database sees
// the same hold, applies nothing again, and does not double-count a resume.
func TestHeldMissionRestart(t *testing.T) {
	ctx := context.Background()
	st, s, a, b := heldSetup(t)
	plant(t, st, b, "ATTEMPT_STARTED", badType)
	plant(t, st, a, "ATTEMPT_FINISHED", waitEvent)
	plant(t, st, a, "MISSION_RESUMED", resumedEvent)
	if _, err := s.consumeOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := New(st, s.cfg, s.ln)
	for i := 0; i < 3; i++ {
		if n, err := s2.consumeOutbox(ctx); err != nil || n != 0 {
			t.Fatalf("restart tick %d: n=%d err=%v", i, n, err)
		}
	}
	if !hasHold(t, st, b) || counter(t, st) != 1 {
		t.Fatalf("after restart: held=%v counter=%d", hasHold(t, st, b), counter(t, st))
	}
}
