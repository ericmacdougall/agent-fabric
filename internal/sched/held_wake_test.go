package sched

import (
	"context"
	"testing"
)

// NEG-16 (wake half), spec v4.3: while a mission is held, an EXITED job whose
// unit is gone stays woken=false (the wake sweep excludes held missions in
// SQL); once the hold is released the wake is delivered exactly once.
func TestHeldExitedJobWokenOnceAfterRelease(t *testing.T) {
	ctx := context.Background()
	st, ln, cfg := ownerSetup(t, "m_hw")
	if _, err := st.Pool().Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state, ended_at)
		VALUES ('j_hw', 'm_hw', 'r_hw_1', 1, '[]', '/x', 'EXITED', now())`); err != nil {
		t.Fatal(err)
	}
	woken := func() bool {
		var w bool
		if err := st.Pool().QueryRow(ctx, "SELECT woken FROM jobs WHERE id = 'j_hw'").Scan(&w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	s := New(st, cfg, ln)
	s.SetProber(&fakeProber{known: true, jobAlive: false})
	if err := st.HoldMission(ctx, st.Pool(), "m_hw"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s.parked = nil
		n, err := s.wakeExitedJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if woken() || n != 0 {
			t.Fatalf("an EXITED job of a held mission was woken (n=%d)", n)
		}
		// The sweep must not even try (no refused command per tick, no park).
		if got := s.parkedIDs(); len(got) != 0 {
			t.Fatalf("the sweep parked %v: it touched a held mission", got)
		}
	}
	if _, err := st.Pool().Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = 'sched.hold.m_hw'`); err != nil {
		t.Fatal(err)
	}
	s.parked = nil
	if _, err := s.wakeExitedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if !woken() {
		t.Fatal("after the release the job was not woken")
	}
	s.parked = nil
	if n, err := s.wakeExitedJobs(ctx); err != nil || n != 0 {
		t.Fatalf("a second sweep woke %d jobs (%v); the wake must be delivered once", n, err)
	}
}
