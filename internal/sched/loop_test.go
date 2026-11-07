package sched

import (
	"context"
	"encoding/json"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// Red-team F1 on f54d526: an attempt abandoned before launch made the
// mission eligible again at once, and Tick reported work, so Run re-ticked
// with no wait: a hot loop that appended START and FINISH to append-only
// history. These pin bounded writes for the two triggers found.

func counts(t *testing.T, st *store.Store, id domain.MissionID) (attempts, events int) {
	t.Helper()
	if err := st.Pool().QueryRow(context.Background(), `SELECT (SELECT count(*) FROM attempts WHERE mission_id = $1),
		(SELECT count(*) FROM events WHERE mission_id = $1)`, string(id)).Scan(&attempts, &events); err != nil {
		t.Fatal(err)
	}
	return
}

// Gate on, resource full: no attempt at all while it stays full (N2); before
// N2 it was one per backoff window, and before F1 one per Tick.
func TestSaturatedGateDoesNotHotLoop(t *testing.T) {
	st, ln, cfg := gatedSetup(t, "m_hl1", 0)
	s := New(st, cfg, ln)
	for i := 0; i < 20; i++ {
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a, e := counts(t, st, "m_hl1")
	if a != 0 || ln.starts != 0 {
		t.Fatalf("20 ticks with the resource full: %d attempts, %d events, %d starts; want 0 attempts, 0 starts", a, e, ln.starts)
	}
	// Work is judged on dispatch: the outbox may still be handing over
	// earlier events on any tick.
	if n, err := s.dispatch(context.Background()); err != nil || n != 0 {
		t.Fatalf("dispatch counted %d missions as work while the mission backs off (%v)", n, err)
	}
}

// Flag off, an event kind this build cannot read: zero writes to that
// mission, and the scheduler does not report it as work.
func TestUnknownEventKindMissionGetsNoWrites(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_hl2")
	ctx := context.Background()
	payload, _ := json.Marshal(domain.Event{Kind: "FUTURE_KIND", Mission: "m_hl2", Seq: 2})
	if _, err := st.Pool().Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ('m_hl2', 2, 'FUTURE_KIND', 'newer', 'c_future', $1)`, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE missions SET seq = 2, state = jsonb_set(state, '{seq}', '2') WHERE id = 'm_hl2'`); err != nil {
		t.Fatal(err)
	}
	_, before := counts(t, st, "m_hl2")
	var cmds0 int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM commands WHERE mission_id = 'm_hl2'").Scan(&cmds0) //nolint:errcheck
	s := New(st, cfg, ln)
	if n, err := s.dispatch(ctx); err != nil || n != 0 {
		t.Fatalf("first pass counted %d missions as work on a mission it cannot read (%v)", n, err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		// The outbox may still hand over the admission on an early tick, so
		// work is judged on dispatch alone.
		if n, err := s.dispatch(ctx); err != nil || n != 0 {
			t.Fatalf("pass %d: dispatch counted %d missions as work on a mission it cannot read (%v)", i, n, err)
		}
	}
	a, after := counts(t, st, "m_hl2")
	var cmds int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM commands WHERE mission_id = 'm_hl2'").Scan(&cmds) //nolint:errcheck
	if a != 0 || after != before || cmds != cmds0 || ln.starts != 0 {
		t.Fatalf("writes after an unknown kind: %d attempts, events %d -> %d, commands %d -> %d, %d starts", a, before, after, cmds0, cmds, ln.starts)
	}
}
