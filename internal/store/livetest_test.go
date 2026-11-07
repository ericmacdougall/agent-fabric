package store

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// AC-6 through Postgres: done on a deployed mission without a live test is a
// stored refusal (a retry under the same key replays it, still
// ErrLiveTestMissing); with the live test, the mission is SUCCEEDED and its
// slot is free.
func TestDoneNeedsLiveTestThroughStore(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_lt")
	ok(t, st, ctx, "chat", "a", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_lt"})
	ok(t, st, ctx, "chat", "sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_lt", Head: "sha_lt"})
	f := startAttempt(t, st, ctx, "m_lt", "r_lt_1")
	ok(t, st, ctx, "fabd", "ack", domain.Command{Kind: domain.CmdAckStart, Mission: "m_lt", Attempt: "r_lt_1", Fence: f})
	ok(t, st, ctx, "host", "ev", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Generation: 1, Head: "sha_lt", Receipt: "rcpt"})
	ok(t, st, ctx, "host", "fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rcpt"}}})
	ok(t, st, ctx, "fabd", "rec", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Result: "VERIFIED"})
	ok(t, st, ctx, "chat", "acc", domain.Command{Kind: domain.CmdAccept, Mission: "m_lt", Attempt: "r_lt_1"})
	ok(t, st, ctx, "fabd", "del", domain.Command{Kind: domain.CmdDeliver, Mission: "m_lt", Attempt: "r_lt_1", Generation: 1})
	before, err := st.SlotsHeld(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := domain.Command{Kind: domain.CmdCompleteMission, Mission: "m_lt"}
	for i := 0; i < 2; i++ { // the second is a replay of the stored refusal
		if _, err := st.Execute(ctx, "chat", "done-early", done); !errors.Is(err, domain.ErrLiveTestMissing) {
			t.Fatalf("try %d: done without a live test: %v, want ErrLiveTestMissing", i, err)
		}
	}
	ok(t, st, ctx, "chat", "live", domain.Command{Kind: domain.CmdRecordLiveTest, Mission: "m_lt", Generation: 1, Head: "sha_lt",
		Host: "h_t2", Target: "https://staging.example.test", Receipt: "rcpt_live"})
	ok(t, st, ctx, "chat", "done", done)
	m, err := st.Mission(ctx, "m_lt")
	if err != nil {
		t.Fatal(err)
	}
	if m.Terminal != domain.TermSucceeded || m.Delivery != domain.DelivLiveVerified {
		t.Fatalf("after done: terminal %s, delivery %s", m.Terminal, m.Delivery)
	}
	if after, _ := st.SlotsHeld(ctx); after != before-1 {
		t.Fatalf("slots held %d -> %d, want one freed", before, after)
	}
	if r, err := st.Rebuild(ctx, "m_lt"); err != nil || r.Terminal != domain.TermSucceeded {
		t.Fatalf("rebuild: %s %v", r.Terminal, err)
	}
}
