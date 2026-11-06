package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// Review F7: each test here kills a red-team mutant that survived the
// package tests (R2, R5, R9, R11, R12, R13, R13b, R16) or pins a typed error.

// R2 and R5: an answer replays only with the same text and the same
// provenance, and is bounded to 1..4096 bytes.
func TestAnswerReplayAndBounds(t *testing.T) {
	st, ctx := openTest(t)
	for _, id := range []string{"a_b1", "a_b2"} {
		if err := st.CreateAsk(ctx, Ask{ID: id, Kind: "decision", Question: "q", Source: "inferred: test"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AnswerAsk(ctx, "a_b1", "yes", "chat", userSrc); err != nil {
		t.Fatal(err)
	}
	other := `user:11111111-2222-4333-8444-555555555555/66666666-7777-4888-9999-000000000001 "yes"`
	if _, err := st.AnswerAsk(ctx, "a_b1", "yes", "chat", other); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("same answer, different source: want a conflict, got %v", err)
	}
	if a, _ := st.GetAsk(ctx, "a_b1"); a.AnswerSrc != userSrc {
		t.Fatalf("source overwritten: %+v", a)
	}
	if _, err := st.AnswerAsk(ctx, "a_b2", strings.Repeat("x", 4097), "chat", userSrc); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("4097-byte answer: want refused, got %v", err)
	}
	if a, _ := st.GetAsk(ctx, "a_b2"); a.State != "open" {
		t.Fatalf("an oversized answer changed the ask: %+v", a)
	}
	if _, err := st.AnswerAsk(ctx, "a_b2", strings.Repeat("x", 4096), "chat", userSrc); err != nil {
		t.Fatalf("4096-byte answer: %v", err)
	}
}

// R9 and the typed refusal: a capacity keeps its unit, and a capacity below
// what is held is an ErrInvalidTransition (exit 3), not a raw 23514.
func TestSetCapacityRefusals(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rcap", 10)
	f := liveAttempt(t, st, ctx, "m_cap", "r_cap")
	if _, err := hold(st, ctx, "cap-1", "r_cap", f, time.Minute, item("rcap", 6)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCapacity(ctx, "rcap", "tokens", 10); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("unit change: want refused, got %v", err)
	}
	var unit string
	if err := st.pool.QueryRow(ctx, "SELECT unit FROM resource_capacities WHERE resource = 'rcap'").Scan(&unit); err != nil || unit != "slots" {
		t.Fatalf("unit after a refused change: %q %v", unit, err)
	}
	err := st.SetCapacity(ctx, "rcap", "slots", 5)
	var pgErr *pgconn.PgError
	if !errors.Is(err, domain.ErrInvalidTransition) || errors.As(err, &pgErr) {
		t.Fatalf("capacity below held: want a typed refusal, got %v", err)
	}
	if c, h, _ := st.Capacity(ctx, "rcap"); c != 10 || h != 6 {
		t.Fatalf("capacity %d held %d after a refused lowering", c, h)
	}
	if err := st.SetCapacity(ctx, "rcap", "slots", 6); err != nil {
		t.Fatalf("capacity equal to held: %v", err)
	}
}

// R11 and R12: a hold's TTL is at most 24 hours and it names at most 16
// resources; both refusals are typed and hold nothing.
func TestHoldBounds(t *testing.T) {
	st, ctx := openTest(t)
	f := liveAttempt(t, st, ctx, "m_hb", "r_hb")
	var items []HoldItem
	for i := 0; i < MaxHoldItems+1; i++ {
		r := fmt.Sprintf("rh%02d", i)
		capacity(t, st, ctx, r, 5)
		items = append(items, item(r, 1))
	}
	if _, err := hold(st, ctx, "hb-ttl", "r_hb", f, MaxHoldTTL+time.Second, items[0]); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("TTL over 24h: want refused, got %v", err)
	}
	if _, err := hold(st, ctx, "hb-items", "r_hb", f, time.Minute, items...); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("17 items: want refused, got %v", err)
	}
	if h := held(t, st, ctx, "rh00"); h != 0 {
		t.Fatalf("held %d after refused holds", h)
	}
	if _, err := hold(st, ctx, "hb-ok", "r_hb", f, MaxHoldTTL, items[:MaxHoldItems]...); err != nil {
		t.Fatalf("16 items for 24h: %v", err)
	}
}

// R13, R13b and R16: the history trigger and the one-live index each refuse
// a forged transaction that every other check accepts.
func TestReservationHistoryAndOneLive(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "rhs", 10)
	f := liveAttempt(t, st, ctx, "m_hs", "r_hs")
	r, err := hold(st, ctx, "hs-1", "r_hs", f, time.Minute, item("rhs", 2))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Reservations[0].ID
	if _, err := st.pool.Exec(ctx, "INSERT INTO hold_commands (key, request_sha256) VALUES ('hs-forged', 'x')"); err != nil {
		t.Fatal(err)
	}
	newRow := func(resID string, gen int) string {
		return fmt.Sprintf(`INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, expires_at)
			VALUES ('%s', 'hs-forged', 'r_hs', %d, 'rhs', 'slots', 1, %d, now() + interval '1 minute')`, resID, f, gen)
	}
	cases := []struct {
		name, code string
		stmts      []string
	}{
		// held is fixed up, so only the history trigger sees the missing event.
		{"closed without its terminal event", "23514", []string{
			`UPDATE reservations SET state = 'RELEASED', closed_at = now() WHERE id = '` + id + `'`,
			`UPDATE resource_capacities SET held = held - 2 WHERE resource = 'rhs'`,
		}},
		// The live hold is released properly first, so the one-live index
		// does not refuse the new row before the history trigger can.
		{"born without its HELD event", "23514", []string{
			`UPDATE reservations SET state = 'RELEASED', closed_at = now() WHERE id = '` + id + `'`,
			`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('` + id + `', 2, 'RELEASED')`,
			`UPDATE resource_capacities SET held = held - 2 WHERE resource = 'rhs'`,
			newRow("rv_noev", 7),
			`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'rhs'`,
		}},
		// A second live hold for the same attempt and resource, fully booked.
		{"two live holds for one attempt and resource", "23505", []string{
			newRow("rv_two", 8),
			`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('rv_two', 1, 'HELD')`,
			`UPDATE resource_capacities SET held = held + 1 WHERE resource = 'rhs'`,
		}},
	}
	for _, c := range cases {
		err := inTx(st, c.stmts...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Errorf("%s: want SQLSTATE %s, got %v", c.name, c.code, err)
		}
	}
	if h := held(t, st, ctx, "rhs"); h != 2 {
		t.Fatalf("held %d after refused writes, want 2", h)
	}
}
