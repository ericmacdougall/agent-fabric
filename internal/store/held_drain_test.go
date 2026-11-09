package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/reducer/corpus"
)

// plantGood appends a decodable row (a corpus event of a known kind) to a
// mission's events and outbox, as an older binary would during a rollback.
func plantGood(t *testing.T, st *Store, ctx context.Context, m domain.MissionID, seq int64) {
	t.Helper()
	kind := domain.EventKind(domain.KnownEventKinds()[0])
	payload, err := json.Marshal(corpus.Event(kind, m, seq, 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ($1, $2, $3, 'test', $4, $5::jsonb)`,
		string(m), seq, string(kind), fmt.Sprintf("good-%s-%d", m, seq), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, string(m), seq, string(kind), payload); err != nil {
		t.Fatal(err)
	}
}

// NEG-17 (drain step) and NEG-13 (drain race), spec v4.3.
//
// A hold row sits behind the consumer cursor. A failure injected after a row
// is applied and before the drain's ack commits leaves the row unapplied and
// the hold unmoved; the redelivered rows are applied exactly once. A row
// written for the mission while the drain runs (an older binary) is above the
// cursor the drain was bounded by: the drain does not deliver it, the hold is
// released, and the main consumer delivers it exactly once, in order.
func TestDrainHoldCrashAndRace(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_dr")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_dr"})
	if _, err := st.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS held_probe2 (id bigint NOT NULL, via text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.pool.Exec(context.Background(), `DROP TABLE IF EXISTS held_probe2`) }) //nolint:errcheck
	const consumer = "drtest"
	probe := func(via string) ApplyFunc {
		return func(ctx context.Context, tx pgx.Tx, r OutboxRow) error {
			_, err := tx.Exec(ctx, `INSERT INTO held_probe2 (id, via) VALUES ($1, $2)`, r.ID, via)
			return err
		}
	}
	// Move the main cursor past the mission's rows, then hold the mission
	// behind it (as a hold found by replay or a start does).
	if _, err := st.ConsumeOutbox(ctx, consumer, 100, func(context.Context, pgx.Tx, OutboxRow) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	st.pool.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1`, consumer).Scan(&cursor) //nolint:errcheck
	if cursor == 0 {
		t.Fatal("main cursor did not move")
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ($1, 0)`, holdConsumer("m_dr")); err != nil {
		t.Fatal(err)
	}

	// 1. crash after apply, before the ack commits.
	boom := errors.New("crash after apply")
	crash := func(ctx context.Context, tx pgx.Tx, r OutboxRow) error {
		if err := probe("crashed")(ctx, tx, r); err != nil {
			return err
		}
		return boom
	}
	if _, err := st.DrainHolds(ctx, consumer, 10, crash); !errors.Is(err, boom) {
		t.Fatalf("want the injected failure, got %v", err)
	}
	var n int
	var holdAt int64
	st.pool.QueryRow(ctx, `SELECT count(*) FROM held_probe2`).Scan(&n)                                                  //nolint:errcheck
	st.pool.QueryRow(ctx, `SELECT last_id FROM outbox_cursors WHERE consumer = $1`, holdConsumer("m_dr")).Scan(&holdAt) //nolint:errcheck
	if n != 0 || holdAt != 0 {
		t.Fatalf("the failed drain step was not rolled back: probe rows %d, hold at %d", n, holdAt)
	}

	// 2. a drain with a row written for the mission while it runs.
	plantGood(t, st, ctx, "m_dr", 999) // above the cursor before the drain starts: the drain's bound must exclude it
	planted := false
	racing := func(ctx context.Context, tx pgx.Tx, r OutboxRow) error {
		if !planted {
			planted = true
			plantGood(t, st, context.Background(), "m_dr", 1000)
		}
		return probe("drain")(ctx, tx, r)
	}
	if _, err := st.DrainHolds(ctx, consumer, 10, racing); err != nil {
		t.Fatal(err)
	}
	if hasHoldRow(t, st, "m_dr") {
		t.Fatal("the hold was not released after the drain caught up with its bound")
	}
	var late int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM held_probe2 WHERE id > $1`, cursor).Scan(&late) //nolint:errcheck
	if late != 0 {
		t.Fatalf("the drain delivered %d rows above the cursor it was bounded by", late)
	}

	// 3. the main consumer delivers the late row exactly once.
	for i := 0; i < 3; i++ {
		if _, err := st.ConsumeOutbox(ctx, consumer, 100, probe("main")); err != nil {
			t.Fatal(err)
		}
	}
	var dup, mainRows int
	st.pool.QueryRow(ctx, `SELECT count(*) - count(DISTINCT id) FROM held_probe2`).Scan(&dup)                        //nolint:errcheck
	st.pool.QueryRow(ctx, `SELECT count(*) FROM held_probe2 WHERE via = 'main' AND id > $1`, cursor).Scan(&mainRows) //nolint:errcheck
	if dup != 0 {
		t.Fatalf("%d rows were applied twice", dup)
	}
	if mainRows != 2 {
		t.Fatalf("the two late rows were delivered %d times by the main consumer, want once each", mainRows)
	}
}

func hasHoldRow(t *testing.T, st *Store, m domain.MissionID) bool {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_cursors WHERE consumer = $1`, holdConsumer(m)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}
