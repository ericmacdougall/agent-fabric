package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

func holdIt(t *testing.T, st *Store, ctx context.Context, m domain.MissionID) {
	t.Helper()
	if err := st.HoldMission(ctx, st.pool, m); err != nil {
		t.Fatal(err)
	}
}

func rowCounts(t *testing.T, st *Store, ctx context.Context) string {
	t.Helper()
	var e, c, o, r int
	for q, dst := range map[string]*int{
		`SELECT count(*) FROM events`:       &e,
		`SELECT count(*) FROM commands`:     &c,
		`SELECT count(*) FROM outbox`:       &o,
		`SELECT count(*) FROM reservations`: &r,
	} {
		if err := st.pool.QueryRow(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("events=%d commands=%d outbox=%d reservations=%d", e, c, o, r)
}

// A command to a held mission is refused with the plain sentinel, nothing is
// stored, and the same command id succeeds once the hold is released.
func TestHeldRefusesCommands(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_hr")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_hr"})
	holdIt(t, st, ctx, "m_hr")

	before := rowCounts(t, st, ctx)
	cmd := domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_hr", Attempt: "r_hr", Host: "h_t2", Role: "writer"}
	_, err := st.Execute(ctx, "fabd", "start:r_hr", cmd)
	if !errors.Is(err, domain.ErrStreamHeld) {
		t.Fatalf("want ErrStreamHeld, got %v", err)
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		t.Fatal("the held refusal must be a plain sentinel, never a *store.Refusal")
	}
	if after := rowCounts(t, st, ctx); after != before {
		t.Fatalf("a refused command wrote rows: %s -> %s", before, after)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM outbox_cursors WHERE consumer = 'sched.hold.m_hr'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", "start:r_hr", cmd); err != nil {
		t.Fatalf("the same command after the release: %v", err)
	}
}

// The abandon of an attempt is exempt from the held refusal, and only from it:
// every other check on the command still runs.
func TestHeldAbandonExemptOnlyFromHeldRefusal(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_ha")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ha"})
	fence := startAttempt(t, st, ctx, "m_ha", "r_ha")
	holdIt(t, st, ctx, "m_ha")

	// A stale fence is still refused by the reducer, not by the hold.
	_, err := st.Execute(ctx, "fabd", "finish:bad", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_ha", Attempt: "r_ha", Fence: fence + 7, Outcome: "abandoned", Prelaunch: true})
	if errors.Is(err, domain.ErrStreamHeld) || err == nil {
		t.Fatalf("a stale-fence abandon: want the reducer's refusal, got %v", err)
	}
	if _, err := st.Execute(ctx, "fabd", "finish:r_ha", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_ha", Attempt: "r_ha", Fence: fence, Outcome: "abandoned", Prelaunch: true}); err != nil {
		t.Fatalf("the abandon of a started attempt on a held mission: %v", err)
	}
	// Anything else on the held mission stays refused.
	if _, err := st.Execute(ctx, "fabd", "start:r_ha2", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_ha", Attempt: "r_ha2", Host: "h_t2", Role: "writer"}); !errors.Is(err, domain.ErrStreamHeld) {
		t.Fatalf("start on a held mission: %v", err)
	}
}

// A held mission whose lease passes leaves attempts, events and commands
// unchanged while a healthy mission still expires.
func TestHeldLeaseUntouched(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 100 * time.Millisecond
	for _, id := range []domain.MissionID{"m_hl_a", "m_hl_b"} {
		mk(t, st, ctx, id)
		ok(t, st, ctx, "chat", "admit-"+string(id), domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
	}
	startAttempt(t, st, ctx, "m_hl_a", "r_hl_a")
	startAttempt(t, st, ctx, "m_hl_b", "r_hl_b")
	holdIt(t, st, ctx, "m_hl_a")
	time.Sleep(250 * time.Millisecond)
	var before string
	if err := st.pool.QueryRow(ctx, `SELECT a::text FROM attempts a WHERE id = 'r_hl_a'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	exp, err := st.ExpireLeases(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp) != 1 || exp[0].Attempt != "r_hl_b" {
		t.Fatalf("expired %+v, want only the healthy mission's attempt", exp)
	}
	var state string
	if err := st.pool.QueryRow(ctx, `SELECT state FROM attempts WHERE id = 'r_hl_a'`).Scan(&state); err != nil || state == string(domain.AttemptUnknown) {
		t.Fatalf("held mission's attempt state %q (%v): the sweep touched it", state, err)
	}
	var after string
	if err := st.pool.QueryRow(ctx, `SELECT a::text FROM attempts a WHERE id = 'r_hl_a'`).Scan(&after); err != nil || after != before {
		t.Fatalf("held mission's attempt row changed:\nbefore %s\nafter  %s (%v)", before, after, err)
	}
}

// Two schedulers on one database: each outbox row is applied exactly once.
func TestConsumeOutboxTwoSchedulers(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_ts")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ts"})
	var seen sync.Map
	var applied int64
	var mu sync.Mutex
	apply := func(_ context.Context, _ pgx.Tx, r OutboxRow) error {
		if _, dup := seen.LoadOrStore(r.ID, true); dup {
			return fmt.Errorf("row %d applied twice", r.ID)
		}
		mu.Lock()
		applied++
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := st.ConsumeOutbox(ctx, "twosched", 50, apply); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if applied < 1 {
		t.Fatal("nothing was applied")
	}
}

// A failure between apply and ack leaves the row unapplied and unacked; the
// redelivered row is applied exactly once.
func TestConsumeOutboxCrashBeforeAck(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_cb")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cb"})
	if _, err := st.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS held_probe (id bigint PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.pool.Exec(context.Background(), `DROP TABLE IF EXISTS held_probe`) }) //nolint:errcheck
	boom := errors.New("crash after apply")
	failing := func(ctx context.Context, tx pgx.Tx, r OutboxRow) error {
		if _, err := tx.Exec(ctx, `INSERT INTO held_probe (id) VALUES ($1)`, r.ID); err != nil {
			return err
		}
		return boom
	}
	if _, err := st.ConsumeOutbox(ctx, "crashy", 10, failing); !errors.Is(err, boom) {
		t.Fatalf("want the injected failure, got %v", err)
	}
	var n int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM held_probe`).Scan(&n) //nolint:errcheck
	var cur int64
	st.pool.QueryRow(ctx, `SELECT COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'crashy'), 0)`).Scan(&cur) //nolint:errcheck
	if n != 0 || cur != 0 {
		t.Fatalf("the failed apply was not rolled back with the ack: probe rows %d, cursor %d", n, cur)
	}
	good := func(ctx context.Context, tx pgx.Tx, r OutboxRow) error {
		_, err := tx.Exec(ctx, `INSERT INTO held_probe (id) VALUES ($1)`, r.ID)
		return err
	}
	got, err := st.ConsumeOutbox(ctx, "crashy", 10, good)
	if err != nil || got < 1 {
		t.Fatalf("redelivery: %d rows, %v", got, err)
	}
	var dup int
	st.pool.QueryRow(ctx, `SELECT count(*) - count(DISTINCT id) FROM held_probe`).Scan(&dup) //nolint:errcheck
	if dup != 0 {
		t.Fatal("a row was applied twice")
	}
}

// A mission held by MISSION_PARKED (BLOCKED) is a different thing from a held
// stream: it is not listed or counted as held.
func TestHeldNotMissionParked(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_np")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_np"})
	if _, err := st.pool.Exec(ctx, `UPDATE missions SET state = jsonb_set(state, '{execution}', '"BLOCKED"') WHERE id = 'm_np'`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.HeldCount(ctx); err != nil || n != 0 {
		t.Fatalf("a BLOCKED mission counted as held: %d %v", n, err)
	}
	holdIt(t, st, ctx, "m_np")
	holdIt(t, st, ctx, "m_np") // idempotent: one hold row, counted once
	if n, err := st.HeldCount(ctx); err != nil || n != 1 {
		t.Fatalf("held count %d %v, want 1", n, err)
	}
	list, err := st.Held(ctx)
	if err != nil || len(list) != 1 || list[0].Mission != "m_np" {
		t.Fatalf("held list %+v %v", list, err)
	}
}
