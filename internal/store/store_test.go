package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/connref"
	"fabric/internal/domain"
	"fabric/internal/reducer"
)

// The store tests need a real Postgres (FABRIC_TEST_DSN, an admin DSN on a
// disposable instance). They fail loudly without one: a green run that never
// touched the database would be vacuous (CLAUDE.md: no vacuous passes).
var testDSN string

func TestMain(m *testing.M) { os.Exit(storeTestMain(m)) }

func storeTestMain(m *testing.M) (code int) {
	if helper := os.Getenv("FABRIC_HELPER_DSN"); helper != "" {
		testDSN = helper // the parent keeps its derived reference alive
		return m.Run()
	}
	adminRef, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test admin credential reference unavailable")
		return 2
	}
	defer adminRef.Close()
	admin, err := adminRef.DSN()
	if err != nil || admin == "" {
		fmt.Fprintln(os.Stderr, "FABRIC_TEST_DSN or FABRIC_TEST_DSN_FILE is required for disposable database tests")
		return 2
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test admin connection failed")
		return 2
	}
	defer pool.Close()
	name := fmt.Sprintf("fabric_t_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		fmt.Fprintln(os.Stderr, "create test database failed")
		return 2
	}
	var dbRef *connref.Ref
	defer func() {
		if _, err := pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			fmt.Fprintln(os.Stderr, "drop test database failed")
			code = 2
		}
		if dbRef != nil {
			dbRef.Close()
		}
	}()
	dbRef, err = connref.ForTestDatabase(adminRef, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "derive test database reference failed")
		return 2
	}
	testDSN, err = dbRef.DSN()
	if err != nil {
		return 2
	}
	st, err := Open(ctx, testDSN)
	if err == nil {
		err = st.Migrate(ctx)
		st.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate test database failed")
		return 2
	}
	return m.Run()
}

func openTest(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	st, err := Open(ctx, testDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st, ctx
}

func count(t *testing.T, st *Store, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func mk(t *testing.T, st *Store, ctx context.Context, id domain.MissionID) {
	t.Helper()
	if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#14791", "fabric", "main"); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func ok(t *testing.T, st *Store, ctx context.Context, actor, key string, cmd domain.Command) Response {
	t.Helper()
	resp, err := st.Execute(ctx, domain.ActorID(actor), domain.CommandID(key), cmd)
	if err != nil {
		t.Fatalf("%s %s: %v", cmd.Kind, key, err)
	}
	return resp
}

func startAttempt(t *testing.T, st *Store, ctx context.Context, id domain.MissionID, attempt domain.AttemptID) int64 {
	t.Helper()
	resp := ok(t, st, ctx, "fabd", "start:"+string(attempt), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: id, Attempt: attempt, Host: "h_t2", Role: "writer"})
	return resp.Events[0].Fence
}

func TestMigrateIdempotent(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	if n := count(t, st, ctx, "SELECT count(*) FROM fabric_schema"); n != len(names) || n < 2 {
		t.Fatalf("schema rows = %d, want %d embedded migrations", n, len(names))
	}
}

// Review R1 on 3f38cc3: a migration edited after it was applied is refused,
// not silently skipped.
func TestMigrateRefusesChangedMigration(t *testing.T) {
	st, ctx := openTest(t)
	var name, sum string
	if err := st.pool.QueryRow(ctx, "SELECT name, sha256 FROM fabric_schema ORDER BY name LIMIT 1").Scan(&name, &sum); err != nil {
		t.Fatalf("schema checksum column: %v", err)
	}
	if len(sum) != 64 {
		t.Fatalf("%s recorded checksum %q", name, sum)
	}
	t.Cleanup(func() {
		st.pool.Exec(context.Background(), "UPDATE fabric_schema SET sha256 = $2 WHERE name = $1", name, sum)
	}) //nolint:errcheck
	if _, err := st.pool.Exec(ctx, "UPDATE fabric_schema SET sha256 = $2 WHERE name = $1", name, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("migrate over a changed %s: %v, want a refusal naming it", name, err)
	}
	// A ledger from before checksums (NULL) is stamped, not refused.
	if _, err := st.pool.Exec(ctx, "UPDATE fabric_schema SET sha256 = NULL WHERE name = $1", name); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate over a legacy ledger row: %v", err)
	}
	var got string
	if err := st.pool.QueryRow(ctx, "SELECT sha256 FROM fabric_schema WHERE name = $1", name).Scan(&got); err != nil || got != sum {
		t.Fatalf("legacy row stamped %q, want %q (%v)", got, sum, err)
	}
}

func TestMissionMustNameTicket(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.CreateMission(ctx, "m_noticket", domain.ProviderClaudeCode, "", "", ""); err == nil {
		t.Fatal("mission without a ticket was accepted (R5)")
	}
	if err := st.CreateMission(ctx, "bad id", domain.ProviderClaudeCode, "GoodQA goodqa#1", "", ""); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("bad id: want ErrInvalidID, got %v", err)
	}
}

// I7: same key + same body replays the stored response without running the
// reducer; same key + different body is a conflict; a refusal replays too.
func TestCommandReplay(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	admit := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"}
	first := ok(t, st, ctx, "chat", "k1", admit)
	if first.Replayed || len(first.Events) != 1 || first.Events[0].Seq != 1 {
		t.Fatalf("first execution: %+v", first)
	}
	again := ok(t, st, ctx, "chat", "k1", admit)
	if !again.Replayed || len(again.Events) != 1 || again.Events[0].Seq != 1 || again.Version != first.Version {
		t.Fatalf("replay: %+v", again)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events"); n != 1 {
		t.Fatalf("events after replay = %d, want 1", n)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM outbox"); n != 1 {
		t.Fatalf("outbox after replay = %d, want 1", n)
	}
	// Same key, different body.
	other := admit
	other.Reason = "different bytes"
	if _, err := st.Execute(ctx, "chat", "k1", other); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("same key different body: want ErrIdempotencyConflict, got %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM commands"); n != 1 {
		t.Fatalf("commands after conflict = %d, want 1", n)
	}
	// A different actor with the same command id is a different key.
	_, err := st.Execute(ctx, "host", "k1", admit)
	var ref *Refusal
	if !errors.As(err, &ref) || !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second admission by another actor: want a refusal (already ADMITTED), got %v", err)
	}
	// The refusal itself replays.
	_, err2 := st.Execute(ctx, "host", "k1", admit)
	var ref2 *Refusal
	if !errors.As(err2, &ref2) || !ref2.Response.Replayed || !errors.Is(err2, domain.ErrInvalidTransition) {
		t.Fatalf("replayed refusal: got %v (replayed=%v)", err2, ref2 != nil && ref2.Response.Replayed)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events"); n != 1 {
		t.Fatalf("events after refusals = %d, want 1", n)
	}
}

// Sixteen concurrent same-key writers converge on one stored result.
func TestConcurrentSameKey(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	cmd := domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_1", Attempt: "r_1", Host: "h_t2", Role: "writer"}
	const n = 16
	var wg sync.WaitGroup
	results := make([]Response, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.Execute(ctx, "fabd", "start-r1", cmd)
		}(i)
	}
	wg.Wait()
	executed := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if !results[i].Replayed {
			executed++
		}
		if len(results[i].Events) != 1 || results[i].Events[0].Fence != 1 || results[i].Events[0].Seq != 2 {
			t.Fatalf("writer %d: %+v", i, results[i].Events)
		}
	}
	if executed != 1 {
		t.Fatalf("reducer ran %d times for one key, want 1", executed)
	}
	// Every racer, winner or replay, gets the same bytes (lost-response protocol).
	for i := 1; i < n; i++ {
		if len(results[i].Raw) == 0 || string(results[i].Raw) != string(results[0].Raw) {
			t.Fatalf("writer %d bytes differ from writer 0:\n%s\n%s", i, results[i].Raw, results[0].Raw)
		}
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM attempts"); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	// Kimi P3.0: one receipt, one event, one outbox row for the key.
	if c, e, o := count(t, st, ctx, "SELECT count(*) FROM commands WHERE command_id = 'start-r1'"),
		count(t, st, ctx, "SELECT count(*) FROM events WHERE command_id = 'start-r1'"),
		count(t, st, ctx, "SELECT count(*) FROM outbox o JOIN events e ON e.mission_id = o.mission_id AND e.seq = o.seq WHERE e.command_id = 'start-r1'"); c != 1 || e != 1 || o != 1 {
		t.Fatalf("for one key: %d command rows, %d events, %d outbox rows; want 1/1/1", c, e, o)
	}
	// Racers with a changed body under the stored key all conflict and add nothing.
	changed := cmd
	changed.Host = "h_other"
	var conflicts int32
	var cwg sync.WaitGroup
	for i := 0; i < n; i++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			if _, err := st.Execute(ctx, "fabd", "start-r1", changed); errors.Is(err, domain.ErrIdempotencyConflict) {
				atomic.AddInt32(&conflicts, 1)
			}
		}()
	}
	cwg.Wait()
	if conflicts != n || count(t, st, ctx, "SELECT count(*) FROM events WHERE mission_id = 'm_1'") != 2 {
		t.Fatalf("changed-body racers: %d of %d conflicted; events %d", conflicts, n, count(t, st, ctx, "SELECT count(*) FROM events WHERE mission_id = 'm_1'"))
	}
}

// AC-1 / I3: the cap is a transaction. Eight missions admit at once with cap
// 4; exactly 4 are admitted. The beforeCommit barrier makes the no-lock mutant
// (M12) fail deterministically: without the slots lock every writer passes the
// count check before any commits.
func TestCapRace(t *testing.T) {
	st, ctx := openTest(t)
	const n = 8
	for i := 0; i < n; i++ {
		mk(t, st, ctx, domain.MissionID(fmt.Sprintf("m_%d", i)))
	}
	var mu sync.Mutex
	arrived := 0
	st.hooks.beforeCommit = func(cmd domain.Command) error {
		mu.Lock()
		arrived++
		mu.Unlock()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			mu.Lock()
			all := arrived >= n
			mu.Unlock()
			if all {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
	var wg sync.WaitGroup
	var admitted, refused int32
	var amu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := domain.MissionID(fmt.Sprintf("m_%d", i))
			_, err := st.Execute(ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
			amu.Lock()
			defer amu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, domain.ErrOverCapacity):
				refused++
			default:
				t.Errorf("mission %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	if admitted != 4 || refused != 4 {
		t.Fatalf("admitted=%d refused=%d, want 4/4", admitted, refused)
	}
	if held, _ := st.SlotsHeld(ctx); held != 4 {
		t.Fatalf("slots held = %d, want 4", held)
	}
	st.hooks.beforeCommit = nil
	// Release an admitted one, then admit a refused one under a fresh key.
	var holder, waiting domain.MissionID
	for i := 0; i < n; i++ {
		m, _ := st.Mission(ctx, domain.MissionID(fmt.Sprintf("m_%d", i)))
		if m.Execution == domain.ExecAdmitted && holder == "" {
			holder = m.ID
		}
		if m.Execution == domain.ExecIdle && waiting == "" {
			waiting = m.ID
		}
	}
	ok(t, st, ctx, "chat", "rel", domain.Command{Kind: domain.CmdRelease, Mission: holder})
	ok(t, st, ctx, "chat", "admit2", domain.Command{Kind: domain.CmdAdmitMission, Mission: waiting})
	if held, _ := st.SlotsHeld(ctx); held != 4 {
		t.Fatalf("slots held after release+admit = %d, want 4", held)
	}
}

// I1 against Postgres: a stale fence is refused, stored as an observation,
// and leaves the snapshot, the log and the attempts row untouched.
func TestStaleFence(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	before, _ := st.Mission(ctx, "m_1")
	events := count(t, st, ctx, "SELECT count(*) FROM events")
	_, err := st.Execute(ctx, "host", "fin-stale", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence - 1, Outcome: "checkpoint"})
	if !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale finish: want ErrStaleFence, got %v", err)
	}
	after, _ := st.Mission(ctx, "m_1")
	if reducer.Digest(before) != reducer.Digest(after) {
		t.Fatal("stale fence changed the snapshot")
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events"); n != events {
		t.Fatalf("stale fence appended events: %d -> %d", events, n)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM stale_observations WHERE attempt_id = 'r_1' AND fence = $1", fence-1); n != 1 {
		t.Fatalf("stale observation rows = %d, want 1", n)
	}
	if err := st.Heartbeat(ctx, "r_1", fence-1); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale heartbeat: want ErrStaleFence, got %v", err)
	}
	if err := st.Heartbeat(ctx, "r_1", fence); err != nil {
		t.Fatalf("live heartbeat: %v", err)
	}
	// After the attempt ends, its fence is dead for every later message.
	ok(t, st, ctx, "host", "fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence, Outcome: "checkpoint"})
	if err := st.Heartbeat(ctx, "r_1", fence); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("heartbeat after finish: want ErrInvalidTransition, got %v", err)
	}
	if err := st.Heartbeat(ctx, "r_none", 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("heartbeat unknown attempt: want ErrNotFound, got %v", err)
	}
}

// FB-4: lease expiry by database time moves the attempt to UNKNOWN, never to
// a relaunch; a heartbeat keeps it alive; the sweep is a bounded, replayable
// command.
func TestLeaseExpiry(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 150 * time.Millisecond
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")

	// Heartbeats hold the lease.
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		if err := st.Heartbeat(ctx, "r_1", fence); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	if exp, err := st.ExpireLeases(ctx, 10); err != nil || len(exp) != 0 {
		t.Fatalf("expired while heartbeating: %v %v", exp, err)
	}

	time.Sleep(250 * time.Millisecond)
	exp, err := st.ExpireLeases(ctx, 10)
	if err != nil || len(exp) != 1 || exp[0].Attempt != "r_1" {
		t.Fatalf("expire: %v %v", exp, err)
	}
	m, _ := st.Mission(ctx, "m_1")
	if m.Attempts["r_1"].State != domain.AttemptUnknown {
		t.Fatalf("attempt after expiry = %s, want UNKNOWN", m.Attempts["r_1"].State)
	}
	// A second sweep finds nothing and changes nothing (replayable command id).
	if exp, err := st.ExpireLeases(ctx, 10); err != nil || len(exp) != 0 {
		t.Fatalf("second sweep: %v %v", exp, err)
	}
	// No relaunch while UNKNOWN.
	_, err = st.Execute(ctx, "fabd", "start:r_2", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_1", Attempt: "r_2", Host: "h_t2", Role: "writer"})
	if !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("relaunch during UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	// Reconcile, then a new fence.
	ok(t, st, ctx, "fabd", "rec:r_1", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence, Result: "FAILED_DEFINITE"})
	if f2 := startAttempt(t, st, ctx, "m_1", "r_2b"); f2 != fence+1 {
		t.Fatalf("new fence = %d, want %d", f2, fence+1)
	}
	var state string
	if err := st.pool.QueryRow(ctx, "SELECT state FROM attempts WHERE id = 'r_1'").Scan(&state); err != nil || state != "FAILED_DEFINITE" {
		t.Fatalf("attempts row for r_1 = %q (%v)", state, err)
	}
}

// I8: events are gap-free per mission, and a replay of the log equals the
// snapshot after a scripted flow through every table.
func TestEventSeqGapFreeAndRebuild(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	mk(t, st, ctx, "m_2")
	ok(t, st, ctx, "chat", "a1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	ok(t, st, ctx, "chat", "a2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"})
	ok(t, st, ctx, "chat", "sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_1", Head: "sha_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	ok(t, st, ctx, "host", "ack", domain.Command{Kind: domain.CmdAckStart, Mission: "m_1", Attempt: "r_1", Fence: fence})
	ok(t, st, ctx, "host", "ev", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_1", Attempt: "r_1", Fence: fence, Revision: 0, Generation: 1, Head: "sha_1", Receipt: "rcpt_1"})
	eff := ok(t, st, ctx, "host", "eff", domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_1", Attempt: "r_1", Fence: fence, EffectKind: "ticket_comment", Request: json.RawMessage(`{"ticket": 14791, "body": "checkpoint"}`)})
	eid := eff.Events[0].Effect
	ok(t, st, ctx, "fabd", "unk", domain.Command{Kind: domain.CmdEffectUnknown, Mission: "m_1", Effect: eid, Submission: 1})
	ok(t, st, ctx, "fabd", "res", domain.Command{Kind: domain.CmdResolveEffect, Mission: "m_1", Effect: eid, Submission: 1, Result: "CONFIRMED"})
	ok(t, st, ctx, "host", "fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence, Outcome: "wait", WaitKey: "job_1"})
	ok(t, st, ctx, "host", "wake", domain.Command{Kind: domain.CmdWake, Mission: "m_1", Token: "job_1"})
	ok(t, st, ctx, "fabd", "rec", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence, Result: "VERIFIED"})
	// The review path: a second run records current evidence, claims it, is verified, then ACCEPT.
	f2 := startAttempt(t, st, ctx, "m_1", "r_2")
	ok(t, st, ctx, "host", "ev2", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_1", Attempt: "r_2", Fence: f2, Revision: 0, Generation: 1, Head: "sha_1", Receipt: "rcpt_2"})
	ok(t, st, ctx, "host", "fin2", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_2", Fence: f2, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rcpt_2"}}})
	ok(t, st, ctx, "fabd", "rec2", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_1", Attempt: "r_2", Fence: f2, Result: "VERIFIED"})
	ok(t, st, ctx, "chat", "acc", domain.Command{Kind: domain.CmdAccept, Mission: "m_1", Attempt: "r_2"})
	ok(t, st, ctx, "fabd", "del", domain.Command{Kind: domain.CmdDeliver, Mission: "m_1", Attempt: "r_2", Generation: 1})
	ok(t, st, ctx, "chat", "rev", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_1", Reason: "user input"})

	events, err := st.Events(ctx, "m_1")
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
	if len(events) < 14 {
		t.Fatalf("only %d events", len(events))
	}
	m, err := st.Rebuild(ctx, "m_1")
	if err != nil {
		t.Fatal(err)
	}
	// The revised intent revoked the acceptance and, since review M2 on
	// fc8e035, the delivery that rested on it.
	if m.Delivery != domain.DelivNotReady || m.DeliveredGeneration != nil || m.Accepted != nil || m.Version.IntentRevision != 1 {
		t.Fatalf("rebuilt state: delivery=%s accepted=%v version=%+v", m.Delivery, m.Accepted, m.Version)
	}
	if _, err := st.Rebuild(ctx, "m_2"); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := st.pool.QueryRow(ctx, "SELECT seq FROM missions WHERE id = 'm_1'").Scan(&seq); err != nil || seq != int64(len(events)) {
		t.Fatalf("missions.seq = %d, events = %d (%v)", seq, len(events), err)
	}
}

func TestOutboxPagesAndCursor(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "a1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	for i := 0; i < 9; i++ {
		ok(t, st, ctx, "host", fmt.Sprintf("w%d", i), domain.Command{Kind: domain.CmdWake, Mission: "m_1", Token: fmt.Sprintf("job_%d", i)})
	}
	// The xid8 guard (H5) hides rows newer than any in-progress transaction,
	// and xids are cluster-wide: a transaction in another package's test
	// database can hold this page back briefly. Consumers poll after a hint,
	// so the test polls too (bounded), then asserts the exact page.
	var page []OutboxRow
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		page, err = st.OutboxPage(ctx, "poster", 4)
		if err != nil || len(page) == 4 || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || len(page) != 4 || page[0].ID != 1 || page[3].ID != 4 {
		t.Fatalf("page 1: %v %v", page, err)
	}
	if err := st.AckOutbox(ctx, "poster", 4); err != nil {
		t.Fatal(err)
	}
	if err := st.AckOutbox(ctx, "poster", 2); err != nil { // backwards: ignored
		t.Fatal(err)
	}
	page, _ = st.OutboxPage(ctx, "poster", 4)
	if len(page) != 4 || page[0].ID != 5 {
		t.Fatalf("page 2 starts at %d (len %d), want 5", page[0].ID, len(page))
	}
	var other []OutboxRow
	// Same xid8 guard as page 1: a cluster-wide in-progress transaction can
	// hold a row back briefly, so the exact-count assertion polls (bounded)
	// instead of sampling once.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		other, _ = st.OutboxPage(ctx, "other-consumer", 100)
		if len(other) == 10 || time.Now().After(deadline) {
			break
		}
	}
	if len(other) != 10 {
		t.Fatalf("independent consumer sees %d rows, want 10", len(other))
	}
	st.AckOutbox(ctx, "poster", 10) //nolint:errcheck
	st.AckOutbox(ctx, "poster", 10) //nolint:errcheck
	if page, _ = st.OutboxPage(ctx, "poster", 4); len(page) != 0 {
		t.Fatalf("drained consumer still sees %d rows", len(page))
	}
}

// FB-5 crash cutpoints for command + event + outbox, in-process.
func TestCrashCutpointsInProcess(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	cmd := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"}

	// Cut before commit: nothing durable; the retry executes once.
	cut := errors.New("cut before commit")
	st.hooks.beforeCommit = func(domain.Command) error { return cut }
	if _, err := st.Execute(ctx, "chat", "k", cmd); !errors.Is(err, cut) {
		t.Fatalf("want the cut error, got %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events") + count(t, st, ctx, "SELECT count(*) FROM commands") + count(t, st, ctx, "SELECT count(*) FROM outbox"); n != 0 {
		t.Fatalf("rows survived a pre-commit cut: %d", n)
	}
	st.hooks.beforeCommit = nil
	resp := ok(t, st, ctx, "chat", "k", cmd)
	if resp.Replayed || len(resp.Events) != 1 {
		t.Fatalf("retry after pre-commit cut: %+v", resp)
	}

	// Cut after commit, before the response reaches the caller.
	mk(t, st, ctx, "m_2")
	cmd2 := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"}
	st.hooks.afterCommit = func(domain.Command) { panic("cut after commit") }
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected the after-commit cut")
			}
		}()
		st.Execute(ctx, "chat", "k2", cmd2) //nolint:errcheck
	}()
	st.hooks.afterCommit = nil
	resp = ok(t, st, ctx, "chat", "k2", cmd2)
	if !resp.Replayed || len(resp.Events) != 1 {
		t.Fatalf("retry after post-commit cut: replayed=%v events=%d", resp.Replayed, len(resp.Events))
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events WHERE mission_id = 'm_2'"); n != 1 {
		t.Fatalf("events for m_2 = %d, want 1 (no duplicate after the cut)", n)
	}
	if held, _ := st.SlotsHeld(ctx); held != 2 {
		t.Fatalf("slots held = %d, want 2", held)
	}
}

// FB-5 with a real process crash: the child runs one command and exits hard
// at the cutpoint; the parent retries with the same key.
func TestCrashCutpointsSubprocess(t *testing.T) {
	st, ctx := openTest(t)
	for _, cut := range []string{"before", "after"} {
		t.Run(cut, func(t *testing.T) {
			id := domain.MissionID("m_" + cut)
			mk(t, st, ctx, id)
			child := exec.Command(os.Args[0], "-test.run=^TestHelperCrash$", "-test.count=1")
			child.Env = append(os.Environ(),
				"FABRIC_HELPER_DSN="+testDSN, "FABRIC_HELPER_CUT="+cut, "FABRIC_HELPER_MISSION="+string(id))
			out, err := child.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("child should exit 7 at the %s cut, got %v\n%s", cut, err, out)
			}
			resp := ok(t, st, ctx, "chat", "crash-key", domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
			if cut == "after" && !resp.Replayed {
				t.Fatal("after-commit crash: retry should replay the stored response")
			}
			if cut == "before" && resp.Replayed {
				t.Fatal("before-commit crash: nothing should have been stored")
			}
			if n := count(t, st, ctx, "SELECT count(*) FROM events WHERE mission_id = $1", string(id)); n != 1 {
				t.Fatalf("events after %s crash + retry = %d, want 1", cut, n)
			}
			if n := count(t, st, ctx, "SELECT count(*) FROM commands WHERE mission_id = $1", string(id)); n != 1 {
				t.Fatalf("commands after %s crash + retry = %d, want 1", cut, n)
			}
		})
	}
}

// TestHelperCrash is the child of TestCrashCutpointsSubprocess, never a test
// on its own.
func TestHelperCrash(t *testing.T) {
	cut := os.Getenv("FABRIC_HELPER_CUT")
	if cut == "" {
		t.Skip("crash helper: only runs as a child process")
	}
	ctx := context.Background()
	st, err := Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.MissionID(os.Getenv("FABRIC_HELPER_MISSION"))
	switch cut {
	case "before":
		st.hooks.beforeCommit = func(domain.Command) error { os.Exit(7); return nil }
	case "after":
		st.hooks.afterCommit = func(domain.Command) { os.Exit(7) }
	}
	st.Execute(ctx, "chat", "crash-key", domain.Command{Kind: domain.CmdAdmitMission, Mission: id}) //nolint:errcheck
	t.Fatal("helper did not crash at the cutpoint")
}

// H1 at the store: two identical requests under different command keys are
// two effects (occurrence 1 and 2); a replay of the first key, as after a
// restart, adds nothing.
func TestEffectOccurrencesStore(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	cmd := domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_1", Attempt: "r_1", Fence: fence, EffectKind: "ticket_comment", Request: json.RawMessage(`{"ticket": 14791, "body": "same"}`)}
	first := ok(t, st, ctx, "host", "c1", cmd)
	// An identical request is refused while the first is DISPATCHED (H7); once resolved it is occurrence 2.
	reordered := cmd
	reordered.Request = json.RawMessage(`{"body":"same","ticket":14791}`)
	if _, err := st.Execute(ctx, "host", "c2-early", reordered); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("identical request while DISPATCHED: want ErrDuplicate, got %v", err)
	}
	ok(t, st, ctx, "fabd", "res1", domain.Command{Kind: domain.CmdResolveEffect, Mission: "m_1", Effect: first.Events[0].Effect, Submission: 1, Result: "CONFIRMED"})
	second := ok(t, st, ctx, "host", "c2", reordered)
	if first.Events[0].Occurrence != 1 || second.Events[0].Occurrence != 2 || first.Events[0].RequestSHA != second.Events[0].RequestSHA {
		t.Fatalf("occurrences: %+v / %+v", first.Events[0], second.Events[0])
	}
	replay := ok(t, st, ctx, "host", "c1", cmd)
	if !replay.Replayed || replay.Events[0].Effect != first.Events[0].Effect {
		t.Fatalf("replay of c1: %+v", replay)
	}
	m, _ := st.Mission(ctx, "m_1")
	if len(m.Effects) != 2 {
		t.Fatalf("effects = %d, want 2", len(m.Effects))
	}
	// Same key, reordered body: the canonical hash matches, so it is a replay, not a conflict.
	again := ok(t, st, ctx, "host", "c1", reordered)
	if !again.Replayed {
		t.Fatal("reordered body under the same key should replay (canonical hash)")
	}
}

// F1 (R25): an oversized request is refused at the door, even under a stale
// fence, and nothing is stored.
func TestOversizedRequestNotStored(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	pad := make([]byte, domain.MaxRequestBytes)
	for i := range pad {
		pad[i] = 'x'
	}
	big := append(append([]byte(`{"pad":"`), pad...), []byte(`"}`)...) // valid JSON, over the cap
	_, err := st.Execute(ctx, "host", "big", domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_1", Attempt: "r_1", Fence: fence - 1, EffectKind: "push", Request: big})
	if err == nil || errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("oversized stale request: want a size refusal before the fence check, got %v", err)
	}
	_, err = st.Execute(ctx, "host", "junk", domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_1", Attempt: "r_1", Fence: fence - 1, EffectKind: "push", Request: json.RawMessage(`{not json`)})
	if err == nil || errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("invalid-JSON stale request: want a refusal before the fence check, got %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM stale_observations") + count(t, st, ctx, "SELECT count(*) FROM commands WHERE command_id IN ('big', 'junk')"); n != 0 {
		t.Fatalf("refused requests left %d rows", n)
	}
}

// Catalog: mission specs are idempotent and ordered; asks need a grammatical
// source (AC-10) and a kind that exists (R8: no chores).
func TestCatalogAndAsks(t *testing.T) {
	st, ctx := openTest(t)
	specs := []MissionSpec{
		{Provider: domain.ProviderClaudeCode, ID: "m_b", Name: "second", Ticket: "GoodQA goodqa#2", Priority: 20, Source: "file:/x/active-slots.json"},
		{Provider: domain.ProviderClaudeCode, ID: "m_a", Name: "first", Ticket: "GoodQA goodqa#1", Priority: 10, Source: "inferred: from the queue file -- test"},
	}
	for _, s := range specs {
		if err := st.CreateMissionSpec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	again := specs[0]
	again.Name = "renamed"
	if err := st.CreateMissionSpec(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMissionSpec(ctx, MissionSpec{Provider: domain.ProviderClaudeCode, ID: "m_c", Ticket: "GoodQA goodqa#3", Source: "agent said so"}); err == nil {
		t.Fatal("mission with a non-grammatical source accepted (R12)")
	}
	if err := st.CreateMissionSpec(ctx, MissionSpec{Provider: domain.ProviderClaudeCode, ID: "m_d", Source: "file:/x"}); err == nil {
		t.Fatal("mission without a ticket accepted (R5)")
	}
	ok(t, st, ctx, "chat", "admit-b", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_b"})
	rows, err := st.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "m_b" || rows[0].Name != "second" || rows[1].ID != "m_a" {
		t.Fatalf("order/idempotency: %+v", rows)
	}
	bad := []Ask{
		{ID: "a_1", Kind: "decision", Question: "q", Source: ""},
		{ID: "a_2", Kind: "decision", Question: "q", Source: "user: the agent message said yes"},
		{ID: "a_3", Kind: "chore", Question: "q", Source: "file:/x"},
	}
	for _, a := range bad {
		if err := st.CreateAsk(ctx, a); err == nil {
			t.Fatalf("ask %s accepted: %+v", a.ID, a)
		}
	}
	good := Ask{ID: "a_ok", Mission: "m_a", Kind: "decision", Question: "which host?", Options: []string{"t2-dev", "mac"}, Recommended: "t2-dev",
		Source: "user:baab765b-d85c-46b4-b7b9-588eb912c770/da6b7254-8b81-4ed4-841c-31f79a4a4887 \"lets make a harness\""}
	if err := st.CreateAsk(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAsk(ctx, good); err != nil {
		t.Fatalf("second create should be a no-op: %v", err)
	}
	asks, err := st.ListAsks(ctx, "open")
	if err != nil || len(asks) != 1 || asks[0].Options[1] != "mac" || asks[0].Mission != "m_a" {
		t.Fatalf("asks: %+v %v", asks, err)
	}
}
