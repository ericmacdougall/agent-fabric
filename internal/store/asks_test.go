package store

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

const userSrc = `user:11111111-2222-4333-8444-555555555555/66666666-7777-4888-9999-000000000000 "yes, use t2-dev"`

// AC-10 second half: an answered ask fires the wake and the mission resumes,
// exactly once, however often the answer is repeated.
func TestAskAnswerResumesMissionOnce(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_ask1")
	ok(t, st, ctx, "chat", "admit-ask1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ask1"})
	fence := startAttempt(t, st, ctx, "m_ask1", "r_ask1")
	ok(t, st, ctx, "host", "fin-ask1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_ask1", Attempt: "r_ask1",
		Fence: fence, Outcome: "wait", WaitKey: AskToken("a_host")})
	if err := st.CreateAsk(ctx, Ask{ID: "a_host", Mission: "m_ask1", Kind: "decision", Question: "which host?",
		Recommended: "t2-dev", Source: "inferred: from the brief -- test"}); err != nil {
		t.Fatal(err)
	}
	m, _ := st.Mission(ctx, "m_ask1")
	if m.Execution != domain.ExecWaiting {
		t.Fatalf("setup: want WAITING, got %s", m.Execution)
	}

	// NEG-A: no source, a prose "user:" source and an <agent-message> quoted as user are refused; the ask stays open.
	for _, src := range []string{"", "user: the agent said yes", `<agent-message> "yes"`} {
		if _, err := st.AnswerAsk(ctx, "a_host", "t2-dev", "chat", src); err == nil {
			t.Fatalf("answer with source %q accepted (R12)", src)
		}
	}
	if a, _ := st.GetAsk(ctx, "a_host"); a.State != "open" {
		t.Fatalf("refused answers changed the ask: %+v", a)
	}

	res, err := st.AnswerAsk(ctx, "a_host", "t2-dev", "chat", userSrc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Wake == nil || res.Ask.State != "answered" || res.Ask.AnswerSrc != userSrc || res.Ask.AnsweredBy != "chat" {
		t.Fatalf("first answer: %+v", res)
	}
	m, _ = st.Mission(ctx, "m_ask1")
	if m.Execution == domain.ExecWaiting {
		t.Fatalf("mission still WAITING after the answer")
	}
	resumed := count(t, st, ctx, `SELECT count(*) FROM events WHERE mission_id = 'm_ask1' AND kind = 'MISSION_RESUMED'`)
	if resumed != 1 {
		t.Fatalf("MISSION_RESUMED events: %d, want 1", resumed)
	}
	before := count(t, st, ctx, `SELECT count(*) FROM events WHERE mission_id = 'm_ask1'`)

	again, err := st.AnswerAsk(ctx, "a_host", "t2-dev", "chat", userSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Wake == nil || !again.Wake.Replayed {
		t.Fatalf("repeat answer did not replay: %+v", again)
	}
	// NEG-B: a different answer is a conflict and changes nothing.
	if _, err := st.AnswerAsk(ctx, "a_host", "mac", "chat", userSrc); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("different answer: want conflict, got %v", err)
	}
	if after := count(t, st, ctx, `SELECT count(*) FROM events WHERE mission_id = 'm_ask1'`); after != before {
		t.Fatalf("events grew from %d to %d on replay/conflict", before, after)
	}
	if a, _ := st.GetAsk(ctx, "a_host"); a.Answer != "t2-dev" {
		t.Fatalf("first answer overwritten: %+v", a)
	}
	if err := st.WithdrawAsk(ctx, "a_host", "no longer needed"); err == nil {
		t.Fatal("an answered ask was withdrawn")
	}
}

// A crash between the answer row and the wake leaves an answered ask with no
// wake; RepairAskWakes fires it once, and a second repair finds nothing.
func TestRepairAskWakes(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_ask2")
	if err := st.CreateAsk(ctx, Ask{ID: "a_crash", Mission: "m_ask2", Kind: "permission", Question: "may I?",
		Source: "inferred: from the brief -- test"}); err != nil {
		t.Fatal(err)
	}
	// The cut: the answer committed, the wake never ran.
	if _, err := st.pool.Exec(ctx, `UPDATE asks SET state = 'answered', answer = 'yes', answered_by = 'chat',
		answered_at = now(), answer_source = $1 WHERE id = 'a_crash'`, userSrc); err != nil {
		t.Fatal(err)
	}
	wakes := `SELECT count(*) FROM events WHERE mission_id = 'm_ask2' AND kind = 'WAKE_RECORDED'`
	if n := count(t, st, ctx, wakes); n != 0 {
		t.Fatalf("setup: %d wakes", n)
	}
	n, err := st.RepairAskWakes(ctx, 100)
	if err != nil || n < 1 {
		t.Fatalf("repair: n=%d err=%v", n, err)
	}
	if got := count(t, st, ctx, wakes); got != 1 {
		t.Fatalf("wakes after repair: %d, want 1", got)
	}
	if n, err := st.RepairAskWakes(ctx, 100); err != nil || n != 0 {
		t.Fatalf("second repair: n=%d err=%v, want 0", n, err)
	}
	// Repeating the answer after the repair replays the same wake.
	res, err := st.AnswerAsk(ctx, "a_crash", "yes", "chat", userSrc)
	if err != nil || !res.Replayed || res.Wake == nil || !res.Wake.Replayed {
		t.Fatalf("answer after repair: %+v %v", res, err)
	}
	if got := count(t, st, ctx, wakes); got != 1 {
		t.Fatalf("wakes after re-answer: %d, want 1", got)
	}
}

func TestAskWithdrawAndBounds(t *testing.T) {
	st, ctx := openTest(t)
	if err := st.CreateAsk(ctx, Ask{ID: "a_wd", Kind: "decision", Question: "q", Source: "inferred: x -- test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithdrawAsk(ctx, "a_wd", ""); err == nil {
		t.Fatal("withdrawal without a reason accepted")
	}
	if err := st.WithdrawAsk(ctx, "a_wd", "superseded"); err != nil {
		t.Fatal(err)
	}
	if err := st.WithdrawAsk(ctx, "a_wd", "superseded"); err != nil {
		t.Fatalf("second withdrawal: %v", err)
	}
	if _, err := st.AnswerAsk(ctx, "a_wd", "yes", "chat", userSrc); err == nil {
		t.Fatal("a withdrawn ask was answered")
	}
	if a, _ := st.GetAsk(ctx, "a_wd"); a.State != "withdrawn" || a.Withdrawn != "superseded" {
		t.Fatalf("withdrawn ask: %+v", a)
	}
	// An ask with no mission records the answer and fires no wake.
	if err := st.CreateAsk(ctx, Ask{ID: "a_free", Kind: "decision", Question: "q", Source: "inferred: x -- test"}); err != nil {
		t.Fatal(err)
	}
	if res, err := st.AnswerAsk(ctx, "a_free", "ok", "chat", userSrc); err != nil || res.Wake != nil {
		t.Fatalf("missionless answer: %+v %v", res, err)
	}
	// An id too long for its wake key is refused at creation.
	long := "a_" + string(make([]byte, 0))
	for len(long) < 49 {
		long += "x"
	}
	if err := st.CreateAsk(ctx, Ask{ID: long, Kind: "decision", Question: "q", Source: "inferred: x -- test"}); err == nil {
		t.Fatal("49-character ask id accepted")
	}
	if _, err := st.AnswerAsk(ctx, "a_missing", "ok", "chat", userSrc); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown ask: %v", err)
	}
}
