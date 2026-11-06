package store

import (
	"testing"

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
