package sched

import (
	"context"
	"testing"
	"time"
)

// goodqa#15587 AC-4: with NoDispatch an ADMITTED claude-code mission is never
// started, however many ticks run; without it the same mission is dispatched
// (the control that makes the first half non-vacuous). The other sweeps still
// run: the outbox is consumed.
func TestNoDispatchStartsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, ln, cfg := ownerSetup(t, "m_nodisp")
	cfg.NoDispatch = true
	s := New(st, cfg, ln)
	for i := 0; i < 5; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if n := countWhere(t, st, `SELECT count(*) FROM attempts WHERE mission_id = 'm_nodisp'`); n != 0 {
		t.Fatalf("with NoDispatch %d attempts were created", n)
	}
	var cursor int64
	if err := st.Pool().QueryRow(ctx, `SELECT COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'sched'), 0)`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor == 0 {
		t.Fatal("NoDispatch also stopped the outbox consumer: the cursor never moved")
	}

	// Control: the same mission, dispatch on, is started within a few ticks.
	st2, ln2, cfg2 := ownerSetup(t, "m_withdisp")
	s2 := New(st2, cfg2, ln2)
	for i := 0; i < 5 && countWhere(t, st2, `SELECT count(*) FROM attempts WHERE mission_id = 'm_withdisp'`) == 0; i++ {
		if _, err := s2.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countWhere(t, st2, `SELECT count(*) FROM attempts WHERE mission_id = 'm_withdisp'`); n == 0 {
		t.Fatal("control: with dispatch on the admitted mission was not started, so the test above proves nothing")
	}
	close(ln2.block)
}
