package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// Review F4: the database backstops cover what the Go code must never do,
// including writes that keep held = sum(live) and the history checks true.
// Each case is a whole forged transaction that the 0009 triggers accepted;
// each must now be refused with the named SQLSTATE.
func TestReservationBackstopsMigration0010(t *testing.T) {
	st, ctx := openTest(t)
	capacity(t, st, ctx, "ra", 10)
	capacity(t, st, ctx, "rb", 10)
	f := liveAttempt(t, st, ctx, "m_b2", "r_b2")
	r, err := hold(st, ctx, "b2-1", "r_b2", f, time.Minute, item("ra", 3))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Reservations[0].ID

	// A second attempt that is no longer live (finished waiting).
	fd := liveAttempt(t, st, ctx, "m_b3", "r_b3")
	ok(t, st, ctx, "host", "fin-b3", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_b3", Attempt: "r_b3",
		Fence: fd, Outcome: "wait", WaitKey: "job:b3"})
	var dead string
	if err := st.pool.QueryRow(ctx, "SELECT state FROM attempts WHERE id = 'r_b3'").Scan(&dead); err != nil {
		t.Fatal(err)
	}
	switch domain.AttemptState(dead) {
	case domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning:
		t.Fatalf("setup: r_b3 is still live (%s)", dead)
	}
	if _, err := st.pool.Exec(ctx, "INSERT INTO hold_commands (key, request_sha256) VALUES ('b2-forged', 'x')"); err != nil {
		t.Fatal(err)
	}

	// insert writes a whole forged hold: the row, its HELD event and held.
	gen := int64(8)
	insert := func(resID, attempt string, fence int64, unit, state, closedAt, expires string) []string {
		gen++
		q := []string{
			`INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, state, expires_at, closed_at)
			 VALUES ('` + resID + `', 'b2-forged', '` + attempt + `', ` + itoa(fence) + `, 'rb', '` + unit + `', 2, ` + itoa(gen) + `, '` + state + `', ` + expires + `, ` + closedAt + `)`,
			`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('` + resID + `', 1, 'HELD')`,
		}
		if state == "HELD" {
			return append(q, `UPDATE resource_capacities SET held = held + 2 WHERE resource = 'rb'`)
		}
		return append(q, `INSERT INTO reservation_events (reservation_id, n, state) VALUES ('`+resID+`', 2, '`+state+`')`)
	}
	cases := []struct {
		name, code string
		stmts      []string
	}{
		{"receipt filed under another resource", "23503", []string{
			`UPDATE reservations SET state = 'SETTLED', closed_at = now() WHERE id = '` + id + `'`,
			`INSERT INTO reservation_events (reservation_id, n, state) VALUES ('` + id + `', 2, 'SETTLED')`,
			`UPDATE resource_capacities SET held = held - 3 WHERE resource = 'ra'`,
			`INSERT INTO usage_receipts (resource, receipt_id, reservation_id, actual) VALUES ('rb', 'u-cross', '` + id + `', 1)`,
		}},
		{"unit other than the capacity's", "23503", insert("rv_unit", "r_b2", f, "tokens", "HELD", "NULL", "now() + interval '1 minute'")},
		{"inserted already closed", "23514", insert("rv_closed", "r_b2", f, "slots", "RELEASED", "now()", "now() + interval '1 minute'")},
		{"TTL over 24 hours", "23514", insert("rv_ttl", "r_b2", f, "slots", "HELD", "NULL", "now() + interval '25 hours'")},
		{"expires before it is created", "23514", insert("rv_past", "r_b2", f, "slots", "HELD", "NULL", "now() - interval '1 second'")},
		{"hold for an attempt that is not live", "23514", insert("rv_dead", "r_b3", fd, "slots", "HELD", "NULL", "now() + interval '1 minute'")},
		{"truncate reservation_events", "23001", []string{"TRUNCATE reservation_events CASCADE"}},
		{"truncate usage_receipts", "23001", []string{"TRUNCATE usage_receipts"}},
		{"truncate reservations", "23001", []string{"TRUNCATE reservations CASCADE"}},
		{"truncate hold_commands", "23001", []string{"TRUNCATE hold_commands CASCADE"}},
		{"truncate attempts cascading into reservations", "23001", []string{"TRUNCATE attempts CASCADE"}},
	}
	for _, c := range cases {
		err := inTx(st, c.stmts...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Errorf("%s: want SQLSTATE %s, got %v", c.name, c.code, err)
		}
	}
	if h := held(t, st, ctx, "ra"); h != 3 {
		t.Fatalf("ra held %d after refused writes, want 3", h)
	}
	if h := held(t, st, ctx, "rb"); h != 0 {
		t.Fatalf("rb held %d after refused writes, want 0", h)
	}
	// The real paths still work alongside the backstops.
	if _, err := st.Settle(ctx, UsageReceipt{ReceiptID: "u-b2", Reservation: id, Fence: f, Actual: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("the tests' own Truncate is refused: %v", err)
	}
}

// inTx runs stmts in one transaction and commits; it returns the first error.
func inTx(st *Store, stmts ...string) error {
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
