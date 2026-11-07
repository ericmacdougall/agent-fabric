package store

import (
	"context"
	"errors"
	"testing"

	"fabric/internal/domain"
)

// AC-6 through Postgres: done on a deployed mission without a live test is a
// stored refusal (a retry under the same key replays it, still
// ErrLiveTestMissing); with the live test, the mission is SUCCEEDED and its
// slot is free.
// deployLT drives m_lt to deployed (DELIVERED) at sha_lt, generation 1.
func deployLT(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
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
}

func TestDoneNeedsLiveTestThroughStore(t *testing.T) {
	st, ctx := openTest(t)
	deployLT(t, st, ctx)
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

// Review N1 on aecf3fa: the broker keeps its own effects table and emits no
// reducer events, so done must count that table too.
func TestDoneCountsBrokerEffects(t *testing.T) {
	st, ctx := openTest(t)
	deployLT(t, st, ctx)
	ok(t, st, ctx, "chat", "live", domain.Command{Kind: domain.CmdRecordLiveTest, Mission: "m_lt", Generation: 1, Head: "sha_lt",
		Host: "h_t2", Target: "https://staging.example.test", Receipt: "rcpt_live"})
	for _, state := range []string{"DISPATCHED", "UNKNOWN", "RECONCILING", "QUARANTINED"} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
			VALUES ('e_n1', 'm_lt', 'r_lt_1', 1, 'ticket_comment', 's_n1', 'k_n1', '{}', 'sha', $1)
			ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state`, state); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "chat", domain.CommandID("done-"+state), domain.Command{Kind: domain.CmdCompleteMission, Mission: "m_lt"}); !errors.Is(err, domain.ErrUnknownUnresolved) {
			t.Errorf("done with a broker effect %s: %v, want ErrUnknownUnresolved", state, err)
		}
	}
	// Settled (and never-dispatched) effects do not block.
	if _, err := st.pool.Exec(ctx, `UPDATE effects SET state = 'CONFIRMED' WHERE id = 'e_n1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_n1b', 'm_lt', 'r_lt_1', 1, 'ticket_comment', 's_n1b', 'k_n1b', '{}', 'sha', 'INTENT')`); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "chat", "done", domain.Command{Kind: domain.CmdCompleteMission, Mission: "m_lt"})
}
