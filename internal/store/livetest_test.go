package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fabric/internal/domain"
)

// AC-6 through Postgres: done on a deployed mission without a live test is a
// stored refusal (a retry under the same key replays it, still
// ErrLiveTestMissing); with the live test, the mission is SUCCEEDED and its
// slot is free.
// deployLT drives m_lt to deployed (DELIVERED) at sha_lt, generation 1.
// whileLive, if set, runs while attempt r_lt_1 is live.
func deployLT(t *testing.T, st *Store, ctx context.Context, whileLive func(fence int64)) {
	t.Helper()
	mk(t, st, ctx, "m_lt")
	ok(t, st, ctx, "chat", "a", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_lt"})
	ok(t, st, ctx, "chat", "sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_lt", Head: "sha_lt"})
	f := startAttempt(t, st, ctx, "m_lt", "r_lt_1")
	ok(t, st, ctx, "fabd", "ack", domain.Command{Kind: domain.CmdAckStart, Mission: "m_lt", Attempt: "r_lt_1", Fence: f})
	if whileLive != nil {
		whileLive(f)
	}
	ok(t, st, ctx, "host", "ev", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Generation: 1, Head: "sha_lt", Receipt: "rcpt"})
	ok(t, st, ctx, "host", "fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rcpt"}}})
	ok(t, st, ctx, "fabd", "rec", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_lt", Attempt: "r_lt_1", Fence: f, Result: "VERIFIED"})
	ok(t, st, ctx, "chat", "acc", domain.Command{Kind: domain.CmdAccept, Mission: "m_lt", Attempt: "r_lt_1"})
	ok(t, st, ctx, "fabd", "del", domain.Command{Kind: domain.CmdDeliver, Mission: "m_lt", Attempt: "r_lt_1", Generation: 1})
}

func TestDoneNeedsLiveTestThroughStore(t *testing.T) {
	st, ctx := openTest(t)
	deployLT(t, st, ctx, nil)
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
	// Effects are created while the attempt is live, then moved by the
	// owner through the broker's own steps (0020 guards the rest).
	path := map[string][]string{
		"INTENT":            {},
		"APPROVAL_REQUIRED": {"APPROVAL_REQUIRED"},
		"APPROVED":          {"APPROVED"},
		"DISPATCHED":        {"DISPATCHED"},
		"UNKNOWN":           {"DISPATCHED", "UNKNOWN"},
		"RECONCILING":       {"DISPATCHED", "UNKNOWN", "RECONCILING"},
		"QUARANTINED":       {"DISPATCHED", "UNKNOWN", "QUARANTINED"},
		"CONFIRMED":         {"DISPATCHED", "CONFIRMED"},
		"FAILED_DEFINITE":   {"DISPATCHED", "FAILED_DEFINITE"},
		"NOT_APPLIED":       {"DISPATCHED", "UNKNOWN", "NOT_APPLIED"},
	}
	run := func(states ...string) error {
		t.Helper()
		if err := st.Truncate(ctx); err != nil {
			t.Fatal(err)
		}
		deployLT(t, st, ctx, func(f int64) {
			for i, state := range states {
				id := fmt.Sprintf("e_n1_%d", i)
				if _, err := st.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
					VALUES ($1, 'm_lt', 'r_lt_1', $2, 'ticket_comment', $1, $1, '{}', 'sha', 'INTENT')`, id, f); err != nil {
					t.Fatal(err)
				}
				for _, s := range path[state] {
					if _, err := st.pool.Exec(ctx, "UPDATE effects SET state = $2 WHERE id = $1", id, s); err != nil {
						t.Fatalf("fixture %s -> %s: %v", id, s, err)
					}
				}
			}
		})
		ok(t, st, ctx, "chat", "live", domain.Command{Kind: domain.CmdRecordLiveTest, Mission: "m_lt", Generation: 1, Head: "sha_lt",
			Host: "h_t2", Target: "https://staging.example.test", Receipt: "rcpt_live"})
		_, err := st.Execute(ctx, "chat", "done", domain.Command{Kind: domain.CmdCompleteMission, Mission: "m_lt"})
		return err
	}
	for _, state := range []string{"DISPATCHED", "UNKNOWN", "RECONCILING", "QUARANTINED", "INTENT", "APPROVAL_REQUIRED", "APPROVED"} {
		if err := run(state); !errors.Is(err, domain.ErrUnknownUnresolved) {
			t.Errorf("done with a broker effect %s: %v, want ErrUnknownUnresolved", state, err)
		}
	}
	// Settled effects do not block.
	if err := run("CONFIRMED", "FAILED_DEFINITE", "NOT_APPLIED"); err != nil {
		t.Errorf("done with only settled effects: %v", err)
	}
}
