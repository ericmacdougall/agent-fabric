package store

import (
	"context"
	"errors"
	"testing"
	"time"

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
	if released, err := st.ReleaseOperatorHold(ctx, "m_oph", noAuth); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	if released, err := st.ReleaseOperatorHold(ctx, "m_oph", noAuth); err != nil || released {
		t.Fatalf("second release reported a hold: %v %v", released, err)
	}
	if held, _ := st.OperatorHeld(ctx, "m_oph"); held {
		t.Fatal("still held after release")
	}
	if err := st.OperatorHold(ctx, "bad id"); err == nil {
		t.Fatal("hold accepted a malformed mission id")
	}
}

func noAuth(int64) error { return nil }

func holdGen(t *testing.T, st *Store, ctx context.Context, m string) (int64, bool) {
	t.Helper()
	var g int64
	err := st.pool.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1`, OpHoldPrefix+m).Scan(&g)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return g, true
}

// Spec v16 AC-34: a hold carries a random positive generation that a
// re-hold keeps; release passes the locked generation to authorize, and an
// authorize failure rolls back and keeps the hold.
func TestOperatorHoldGeneration(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_gen")
	if err := st.OperatorHold(ctx, "m_gen"); err != nil {
		t.Fatal(err)
	}
	g1, ok := holdGen(t, st, ctx, "m_gen")
	if !ok || g1 <= 0 {
		t.Fatalf("generation %d %v", g1, ok)
	}
	if err := st.OperatorHold(ctx, "m_gen"); err != nil {
		t.Fatal(err)
	}
	if g, _ := holdGen(t, st, ctx, "m_gen"); g != g1 {
		t.Fatalf("re-hold changed the generation %d -> %d", g1, g)
	}
	boom := errors.New("record not durable")
	if _, err := st.ReleaseOperatorHold(ctx, "m_gen", func(g int64) error {
		if g != g1 {
			t.Errorf("authorize saw %d, want %d", g, g1)
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("release: %v", err)
	}
	if _, ok := holdGen(t, st, ctx, "m_gen"); !ok {
		t.Fatal("a failed authorization released the hold")
	}
}

// Red-team v16 p1: a re-hold that arrives while a release holds the row
// FOR UPDATE waits for it, and after the release commits it leaves a new
// hold with a new generation. With ON CONFLICT DO NOTHING the re-hold
// returns at once, folds into the old generation and the release deletes
// it, so the mission is left unheld.
func TestOperatorReholdWaitsForRelease(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rh")
	if err := st.OperatorHold(ctx, "m_rh"); err != nil {
		t.Fatal(err)
	}
	g1, _ := holdGen(t, st, ctx, "m_rh")
	locked, proceed := make(chan struct{}), make(chan struct{})
	relDone := make(chan error, 1)
	go func() {
		_, err := st.ReleaseOperatorHold(ctx, "m_rh", func(int64) error {
			close(locked)
			<-proceed
			return nil
		})
		relDone <- err
	}()
	<-locked
	holdDone := make(chan error, 1)
	go func() { holdDone <- st.OperatorHold(ctx, "m_rh") }()
	select {
	case err := <-holdDone:
		close(proceed)
		<-relDone
		t.Fatalf("the re-hold did not wait for the release (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(proceed)
	if err := <-relDone; err != nil {
		t.Fatal(err)
	}
	if err := <-holdDone; err != nil {
		t.Fatal(err)
	}
	g2, ok := holdGen(t, st, ctx, "m_rh")
	if !ok {
		t.Fatal("the release deleted the re-hold")
	}
	if g2 == g1 {
		t.Fatal("the re-hold kept the released generation")
	}
}

// Spec v16: a release deletes only the generation it authorized, so a hold
// placed after that release is never removed by it (here a stale
// authorization is replayed against the new row).
func TestReleaseDeletesOnlyItsGeneration(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_sg")
	if err := st.OperatorHold(ctx, "m_sg"); err != nil {
		t.Fatal(err)
	}
	g1, _ := holdGen(t, st, ctx, "m_sg")
	if _, err := st.ReleaseOperatorHold(ctx, "m_sg", noAuth); err != nil {
		t.Fatal(err)
	}
	if err := st.OperatorHold(ctx, "m_sg"); err != nil {
		t.Fatal(err)
	}
	g2, _ := holdGen(t, st, ctx, "m_sg")
	if g2 == g1 {
		t.Fatal("new hold reused the released generation")
	}
	tag, err := st.pool.Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = $1 AND last_id = $2`, OpHoldPrefix+"m_sg", g1)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("the old generation's delete matched the new hold: %v %d", err, tag.RowsAffected())
	}
	if _, ok := holdGen(t, st, ctx, "m_sg"); !ok {
		t.Fatal("new hold gone")
	}
}
