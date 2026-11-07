package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"fabric/internal/domain"
)

// goodqa#15082 T3: one reservation whose close fails must not block the
// rows that sort after it. Both sweeps close every other due row, report
// the failure naming the bad row, and leave only its amount held.
func TestSweepsIsolatePerRowFailures(t *testing.T) {
	sweeps := []struct {
		name  string
		sweep func(*Store, context.Context) (int, error)
	}{
		{"ExpireReservations", func(st *Store, ctx context.Context) (int, error) { return st.ExpireReservations(ctx, 10) }},
		{"ExpireEndedReservations", func(st *Store, ctx context.Context) (int, error) { return st.ExpireEndedReservations(ctx, 10) }},
	}
	for i, c := range sweeps {
		st, ctx := openTest(t)
		res := "rs" + string(rune('a'+i))
		capacity(t, st, ctx, res, 10)
		ma, aa := domain.MissionID("m_sa"+res), domain.AttemptID("r_a_"+res) // sorts first
		mb, ab := domain.MissionID("m_sb"+res), domain.AttemptID("r_b_"+res)
		fa := liveAttempt(t, st, ctx, ma, aa)
		fb := liveAttempt(t, st, ctx, mb, ab)
		if _, err := hold(st, ctx, "h-a-"+res, aa, fa, time.Second, item(res, 3)); err != nil {
			t.Fatal(err)
		}
		if _, err := hold(st, ctx, "h-b-"+res, ab, fb, time.Second, item(res, 2)); err != nil {
			t.Fatal(err)
		}
		ok(t, st, ctx, "fabd", "fin-"+string(aa), domain.Command{Kind: domain.CmdFinishAttempt, Mission: ma, Attempt: aa, Fence: fa, Outcome: "abandoned"})
		ok(t, st, ctx, "fabd", "fin-"+string(ab), domain.Command{Kind: domain.CmdFinishAttempt, Mission: mb, Attempt: ab, Fence: fb, Outcome: "abandoned"})
		var badID string
		if err := st.pool.QueryRow(ctx, `SELECT id FROM reservations WHERE attempt_id = $1`, string(aa)).Scan(&badID); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`CREATE OR REPLACE FUNCTION t3_bad_row() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF OLD.attempt_id LIKE 'r_a_%' THEN
					RAISE EXCEPTION 'injected per-row failure (goodqa#15082 T3)';
				END IF;
				RETURN NEW;
			END $$`,
			`CREATE TRIGGER t3_bad_row BEFORE UPDATE ON reservations FOR EACH ROW EXECUTE FUNCTION t3_bad_row()`,
		} {
			if _, err := st.pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		drop := func() {
			st.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS t3_bad_row ON reservations`) //nolint:errcheck
			st.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS t3_bad_row()`)              //nolint:errcheck
		}
		t.Cleanup(drop)
		time.Sleep(1200 * time.Millisecond) // both holds are due by DB time
		n, err := c.sweep(st, ctx)
		if err == nil || !strings.Contains(err.Error(), badID) {
			t.Errorf("%s: error %v, want one naming %s", c.name, err, badID)
		}
		if n != 1 {
			t.Errorf("%s: closed %d, want 1 (the row after the bad one)", c.name, n)
		}
		if h := held(t, st, ctx, res); h != 3 {
			t.Errorf("%s: held %d, want 3 (only the bad row)", c.name, h)
		}
		drop()
	}
}
