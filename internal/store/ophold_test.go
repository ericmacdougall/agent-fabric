package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// AC-34 (spec v13, owner ruling R1): the undecodable-log drain releases its
// own hold row and never the operator hold, which is not listed as an
// undecodable-log hold; release reports whether a hold existed.
func TestOperatorHoldSurvivesDrain(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_oph")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_oph"})
	if err := st.OperatorHold(ctx, "m_oph"); err != nil {
		t.Fatal(err)
	}
	if err := st.OperatorHold(ctx, "m_oph"); err != nil {
		t.Fatalf("second hold: %v", err)
	}
	if err := st.HoldMission(ctx, st.pool, "m_oph"); err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context, pgx.Tx, OutboxRow) error { return nil }
	for i := 0; i < 3; i++ {
		if _, err := st.DrainHolds(ctx, "sched", 100, noop); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM outbox_cursors WHERE consumer = 'sched.hold.m_oph'`); n != 0 {
		t.Fatal("drain did not release its own hold: the survival check below is vacuous")
	}
	if held, err := st.OperatorHeld(ctx, "m_oph"); err != nil || !held {
		t.Fatalf("operator hold after drain: %v %v", held, err)
	}
	if hs, err := st.Held(ctx); err != nil || len(hs) != 0 {
		t.Fatalf("the operator hold is listed as an undecodable-log hold: %v %v", hs, err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM outbox_cursors WHERE consumer LIKE 'sched.ophold.%'`); n != 1 {
		t.Fatalf("%d operator hold rows", n)
	}
	if released, err := st.ReleaseOperatorHold(ctx, "m_oph"); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	if released, err := st.ReleaseOperatorHold(ctx, "m_oph"); err != nil || released {
		t.Fatalf("second release reported a hold: %v %v", released, err)
	}
	if held, _ := st.OperatorHeld(ctx, "m_oph"); held {
		t.Fatal("still held after release")
	}
	if err := st.OperatorHold(ctx, "bad id"); err == nil {
		t.Fatal("hold accepted a malformed mission id")
	}
}
