package store

import (
	"testing"

	"fabric/internal/domain"
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
