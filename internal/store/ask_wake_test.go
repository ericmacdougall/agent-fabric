package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fabric/internal/domain"
)

// Review F1 (HIGH): the ask: wait tokens belong to the asks actor. A WAKE or
// ACK_WAKE of an ask: token is accepted only from AskActor under the fixed
// key AskWakeKey(id), and only when that ask is answered for that mission.
// Anyone can set FABRIC_ACTOR=asks, so the answered-ask check is the control
// that matters; the actor and key checks keep the replay row unforgeable.
// A refused forgery is never stored, so it cannot poison the real wake's key.
func TestAskWakeReservedForAnsweredAsk(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_aw1")
	mk(t, st, ctx, "m_aw2")
	ok(t, st, ctx, "chat", "admit-aw1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_aw1"})
	fence := startAttempt(t, st, ctx, "m_aw1", "r_aw1")
	ok(t, st, ctx, "host", "fin-aw1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_aw1", Attempt: "r_aw1",
		Fence: fence, Outcome: "wait", WaitKey: AskToken("a_aw1")})
	for _, a := range []Ask{
		{ID: "a_aw1", Mission: "m_aw1", Kind: "decision", Question: "which host?", Source: "inferred: test"},
		{ID: "a_aw2", Mission: "m_aw2", Kind: "decision", Question: "other mission", Source: "inferred: test"},
	} {
		if err := st.CreateAsk(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// a_aw2 is answered, but for another mission: its answer must not wake m_aw1.
	if _, err := st.AnswerAsk(ctx, "a_aw2", "x", "chat", userSrc); err != nil {
		t.Fatal(err)
	}

	stillWaiting := func(when string) {
		t.Helper()
		m, err := st.Mission(ctx, "m_aw1")
		if err != nil {
			t.Fatal(err)
		}
		if m.Execution != domain.ExecWaiting || len(m.Wakes) != 0 {
			t.Fatalf("%s: mission woke (execution %s, wakes %v)", when, m.Execution, m.Wakes)
		}
	}
	wake := func(token string) domain.Command {
		return domain.Command{Kind: domain.CmdWake, Mission: "m_aw1", Token: token}
	}
	forgeries := []struct {
		name, actor, key string
		cmd              domain.Command
	}{
		{"another actor, any key", "chat", "w-aw1", wake(AskToken("a_aw1"))},
		{"another actor, the fixed key", "chat", AskWakeKey("a_aw1"), wake(AskToken("a_aw1"))},
		{"asks actor, wrong key", AskActor, "w-aw1", wake(AskToken("a_aw1"))},
		{"asks actor, fixed key, ask still open", AskActor, AskWakeKey("a_aw1"), wake(AskToken("a_aw1"))},
		{"asks actor, answered ask of another mission", AskActor, AskWakeKey("a_aw2"), wake(AskToken("a_aw2"))},
		{"asks actor, unknown ask", AskActor, AskWakeKey("a_none"), wake(AskToken("a_none"))},
		{"asks actor, not a wake", AskActor, "rel-aw1", domain.Command{Kind: domain.CmdRelease, Mission: "m_aw1"}},
		{"asks actor, non-ask token", AskActor, "w-job", wake("job:j1")},
	}
	cmdsBefore := count(t, st, ctx, `SELECT count(*) FROM commands WHERE mission_id = 'm_aw1'`)
	for _, f := range forgeries {
		if resp, err := st.Execute(ctx, domain.ActorID(f.actor), domain.CommandID(f.key), f.cmd); err == nil {
			t.Fatalf("%s: accepted (%+v)", f.name, resp)
		}
		stillWaiting(f.name)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM commands WHERE mission_id = 'm_aw1'`); n != cmdsBefore {
		t.Fatalf("%d forged commands were stored; a stored refusal would poison the real wake key", n-cmdsBefore)
	}

	// The real answer still wakes the mission, exactly once.
	res, err := st.AnswerAsk(ctx, "a_aw1", "t2-dev", "chat", userSrc)
	if err != nil || res.Wake == nil || res.Wake.Replayed {
		t.Fatalf("real answer: %+v, %v", res, err)
	}
	m, _ := st.Mission(ctx, "m_aw1")
	if m.Execution == domain.ExecWaiting || !m.Wakes[AskToken("a_aw1")] {
		t.Fatalf("real answer did not wake: %+v", m)
	}

	// After the answer, another actor still cannot WAKE (new key) or ACK_WAKE the token.
	for _, f := range []struct {
		name, actor, key string
		cmd              domain.Command
	}{
		{"late wake by another actor", "chat", "w-late", wake(AskToken("a_aw1"))},
		{"asks actor, second key once answered", AskActor, "w-aw1-again", wake(AskToken("a_aw1"))},
		{"ack by another actor", "chat", "ack-aw1", domain.Command{Kind: domain.CmdAckWake, Mission: "m_aw1", Token: AskToken("a_aw1")}},
	} {
		if _, err := st.Execute(ctx, domain.ActorID(f.actor), domain.CommandID(f.key), f.cmd); err == nil {
			t.Fatalf("%s: accepted", f.name)
		}
	}
	if m, _ := st.Mission(ctx, "m_aw1"); !m.Wakes[AskToken("a_aw1")] {
		t.Fatal("another actor acknowledged the ask wake")
	}

	// Tokens outside the ask: namespace are untouched.
	mk(t, st, ctx, "m_aw3")
	ok(t, st, ctx, "chat", "w-plain", domain.Command{Kind: domain.CmdWake, Mission: "m_aw3", Token: "plain"})
	ok(t, st, ctx, "chat", "ack-plain", domain.Command{Kind: domain.CmdAckWake, Mission: "m_aw3", Token: "plain"})
}

// Review F2: a withdrawal that waits on the row lock of an answer in flight
// must not report success once the answer commits. The answer is held open
// in a second transaction; WithdrawAsk blocks on its row lock, the answer
// commits, and the withdrawal must then be refused with the ask answered.
func TestWithdrawRacingAnswerIsRefused(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.CreateAsk(ctx, Ask{ID: "a_wr1", Kind: "decision", Question: "q", Source: "inferred: test"}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE asks SET state = 'answered', answer = 'yes', answered_by = 'chat',
		answered_at = now(), answer_source = $2 WHERE id = $1 AND state = 'open'`, "a_wr1", userSrc); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- st.WithdrawAsk(ctx, "a_wr1", "no longer needed") }()
	waitForLockWaiter(t, st, ctx, "asks")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("withdrawal racing an answer reported success")
	}
	if a, _ := st.GetAsk(ctx, "a_wr1"); a.State != "answered" || a.Answer != "yes" {
		t.Fatalf("ask after the race: %+v", a)
	}
	// A repeat withdrawal of a withdrawn ask stays a success (idempotent).
	if err := st.CreateAsk(ctx, Ask{ID: "a_wr2", Kind: "decision", Question: "q", Source: "inferred: test"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.WithdrawAsk(ctx, "a_wr2", "no longer needed"); err != nil {
			t.Fatalf("withdrawal %d: %v", i, err)
		}
	}
}

// waitForLockWaiter waits until some backend waits on a lock while running a
// statement that mentions table, so a race test interleaves deterministically.
func waitForLockWaiter(t *testing.T, st *Store, ctx context.Context, table string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND datname = current_database() AND query ILIKE '%' || $1 || '%'`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend blocked on a lock touching %s within 10s", table)
}

// waitOnAskMission admits m, starts and finishes an attempt waiting on ask id.
func waitOnAskMission(t *testing.T, st *Store, ctx context.Context, m domain.MissionID, a domain.AttemptID, id string) {
	t.Helper()
	ok(t, st, ctx, "chat", "admit-"+string(m), domain.Command{Kind: domain.CmdAdmitMission, Mission: m})
	fence := startAttempt(t, st, ctx, m, a)
	ok(t, st, ctx, "host", "fin-"+string(a), domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: a,
		Fence: fence, Outcome: "wait", WaitKey: AskToken(id)})
	if err := st.CreateAsk(ctx, Ask{ID: id, Mission: m, Kind: "decision", Question: "q", Source: "inferred: test"}); err != nil {
		t.Fatal(err)
	}
}

// lockMission holds mission m's row lock until the returned func runs.
func lockMission(t *testing.T, st *Store, ctx context.Context, m domain.MissionID) func() {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM missions WHERE id = $1 FOR UPDATE", string(m)); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

// Review F8: once the answer is stored, a failed wake is not a refusal. The
// error must say the answer is recorded and the wake pending, must not
// match the refusal kinds (the CLI maps those to exit 3, "nothing changed"),
// and must carry the stored ask. RepairAskWakes then completes the wake.
func TestAnswerStoredWakePending(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_wp1")
	waitOnAskMission(t, st, ctx, "m_wp1", "r_wp1", "a_wp1")
	unlock := lockMission(t, st, ctx, "m_wp1")
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	res, err := st.AnswerAsk(short, "a_wp1", "yes", "chat", userSrc)
	cancel()
	unlock()
	var wp *WakePendingError
	if !errors.As(err, &wp) || wp.Ask != "a_wp1" {
		t.Fatalf("wake failure after a stored answer: want *WakePendingError, got %v", err)
	}
	for _, kind := range []error{domain.ErrInvalidTransition, domain.ErrNotFound, domain.ErrIdempotencyConflict, domain.ErrInvalidID} {
		if errors.Is(err, kind) {
			t.Fatalf("a stored answer reads as a refusal (%v): %v", kind, err)
		}
	}
	if res.Ask.State != "answered" || res.Ask.Answer != "yes" || res.Wake != nil {
		t.Fatalf("result with a pending wake: %+v", res)
	}
	if a, _ := st.GetAsk(ctx, "a_wp1"); a.State != "answered" {
		t.Fatalf("answer not stored: %+v", a)
	}
	if n, err := st.RepairAskWakes(ctx, 10); err != nil || n != 1 {
		t.Fatalf("repair: %d, %v", n, err)
	}
	if m, _ := st.Mission(ctx, "m_wp1"); m.Execution == domain.ExecWaiting {
		t.Fatal("mission still waiting after the repair")
	}
}

// Review F8: one stuck wake does not hold back the others. Each repair wake
// has its own deadline; the repair continues and reports the failure.
func TestRepairAskWakesNotHeadOfLine(t *testing.T) {
	st, ctx := openTest(t)
	st.repairWakeTimeout = 300 * time.Millisecond
	for _, m := range []domain.MissionID{"m_hl1", "m_hl2"} {
		mk(t, st, ctx, m)
	}
	waitOnAskMission(t, st, ctx, "m_hl1", "r_hl1", "a_hl1")
	waitOnAskMission(t, st, ctx, "m_hl2", "r_hl2", "a_hl2")
	// Both answers stored with their wakes pending; a_hl1 first in repair order.
	for _, id := range []string{"a_hl1", "a_hl2"} {
		m := domain.MissionID("m_hl" + id[len(id)-1:])
		unlock := lockMission(t, st, ctx, m)
		short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := st.AnswerAsk(short, id, "yes", "chat", userSrc)
		cancel()
		unlock()
		var wp *WakePendingError
		if !errors.As(err, &wp) {
			t.Fatalf("setup %s: %v", id, err)
		}
	}
	unlock := lockMission(t, st, ctx, "m_hl1")
	defer unlock()
	// The whole repair gets 5 s; the stuck head must use only its own 300 ms.
	whole, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := st.RepairAskWakes(whole, 10)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "a_hl1") {
		t.Fatalf("repair with a stuck head: %d, %v (want 1 and an error naming a_hl1)", n, err)
	}
	if m, _ := st.Mission(ctx, "m_hl2"); m.Execution == domain.ExecWaiting {
		t.Fatal("the second wake was held back by the first")
	}
}
