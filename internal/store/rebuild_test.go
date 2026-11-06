package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/reducer"
)

// Kimi projection-analysis (defect 1): Rebuild read the event log and the
// mission snapshot in two statements, so a writer committing between them
// produced a spurious digest mismatch although every committed state was
// valid. Both reads now share one REPEATABLE READ snapshot: the rebuild sees
// the state before the writer, whole, and reports no mismatch.
func TestRebuildConcurrentWriterUsesOneSnapshot(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rs1")
	ok(t, st, ctx, "chat", "admit-rs1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rs1"})
	var writerErr error
	st.hooks.afterEventsRead = func() {
		_, writerErr = st.Execute(ctx, "chat", "wake-rs1", domain.Command{Kind: domain.CmdWake, Mission: "m_rs1", Token: "job:rs1"})
	}
	m, events, err := st.VerifiedSnapshot(ctx, "m_rs1")
	st.hooks.afterEventsRead = nil
	if writerErr != nil {
		t.Fatalf("writer between the reads: %v", writerErr)
	}
	if err != nil {
		t.Fatalf("rebuild with a writer between its reads: %v", err)
	}
	if len(events) != 1 || m.Seq != 1 || m.Wakes["job:rs1"] {
		t.Fatalf("rebuild mixed snapshots: %d events, seq %d, wakes %v", len(events), m.Seq, m.Wakes)
	}
	// The writer did commit: a fresh rebuild sees it.
	m, events, err = st.VerifiedSnapshot(ctx, "m_rs1")
	if err != nil || len(events) != 2 || !m.Wakes["job:rs1"] {
		t.Fatalf("fresh rebuild after the writer: %d events, wakes %v, %v", len(events), m.Wakes, err)
	}
	if _, err := st.Rebuild(ctx, "m_rs1"); err != nil {
		t.Fatalf("Rebuild wrapper: %v", err)
	}
}

// A verified read changes nothing and still reports a corrupt snapshot.
func TestVerifiedSnapshotIsReadOnlyAndDetectsCorruption(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rs2")
	ok(t, st, ctx, "chat", "admit-rs2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rs2"})
	counts := func() [3]int {
		return [3]int{count(t, st, ctx, "SELECT count(*) FROM commands"), count(t, st, ctx, "SELECT count(*) FROM events"),
			count(t, st, ctx, "SELECT count(*) FROM outbox")}
	}
	before := counts()
	if _, _, err := st.VerifiedSnapshot(ctx, "m_rs2"); err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before {
		t.Fatalf("a verified read wrote rows: %v -> %v", before, after)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE missions SET state = jsonb_set(state, '{execution}', '"RUNNING"') WHERE id = 'm_rs2'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.VerifiedSnapshot(ctx, "m_rs2"); err == nil {
		t.Fatal("a corrupt snapshot verified")
	}
}

// Kimi compatibility analysis: a log holding an event kind this binary does
// not know is refused, even when the stored snapshot agrees with a
// permissive fold, and no partial state is served.
func TestVerifiedSnapshotRejectsUnknownEvent(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rs3")
	log := []domain.Event{
		{Kind: domain.EvMissionAdmitted, Mission: "m_rs3", Seq: 1},
		{Kind: "FUTURE_PUBLICATION", Mission: "m_rs3", Seq: 2},
		{Kind: domain.EvWakeRecorded, Mission: "m_rs3", Seq: 3, Token: "after-future"},
	}
	m := domain.NewMission("m_rs3")
	for _, e := range log {
		payload, _ := json.Marshal(e)
		if _, err := st.pool.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload)
			VALUES ($1, $2, $3, 'test', $4, $5)`, "m_rs3", e.Seq, string(e.Kind), fmt.Sprintf("c%d", e.Seq), payload); err != nil {
			t.Fatal(err)
		}
		m = reducer.Apply(m, e) // the permissive fold: the snapshot agrees with it
	}
	state, _ := json.Marshal(m)
	if _, err := st.pool.Exec(ctx, "UPDATE missions SET state = $2, seq = 3 WHERE id = $1", "m_rs3", state); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, events, err := st.VerifiedSnapshot(ctx, "m_rs3")
		if !errors.Is(err, domain.ErrUnsupportedEvent) || events != nil || got.ID != "" {
			t.Fatalf("call %d: want ErrUnsupportedEvent with no state, got mission %q, %d events, %v", i, got.ID, len(events), err)
		}
	}
}
