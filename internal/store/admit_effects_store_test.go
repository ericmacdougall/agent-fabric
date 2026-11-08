package store

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15166 (Kimi K10), through the store: the unsettled-effect count is
// computed for ADMIT_MISSION, so a mission whose ended attempt left
// an effect with an unknown outcome cannot be admitted, even by an operator,
// and the refusal changes nothing. Once the effect settles, admit works.
func TestAdmitCountsUnsettledEffects(t *testing.T) {
	st, ctx := openTest(t)
	m := domain.MissionID("m_k10")
	f := liveAttempt(t, st, ctx, m, "r_k10_1")
	ok(t, st, ctx, "fabd", "ack:r_k10_1", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: "r_k10_1", Fence: f})
	if _, err := st.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_k10', 'm_k10', 'r_k10_1', $1, 'ticket_comment', 'e_k10', 'e_k10', '{}', 'sha', 'INTENT')`, f); err != nil {
		t.Fatal(err)
	}
	// The attempt ends (abandoned, a definite failure) leaving its intent
	// unsettled, and the slot is released: only the effect stands in the
	// way of a fresh admission.
	ok(t, st, ctx, "host", "fin-r_k10_1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: "r_k10_1", Fence: f, Outcome: "abandoned", Reason: "test"})
	ok(t, st, ctx, "chat", "rel-m_k10", domain.Command{Kind: domain.CmdRelease, Mission: m})
	before, err := st.Mission(ctx, m)
	if err != nil || before.Execution.HoldsSlot() {
		t.Fatalf("setup: %v %s, want a mission that holds no slot", err, before.Execution)
	}
	for _, actor := range []domain.ActorID{"chat", "op:eric"} {
		_, err := st.Execute(ctx, actor, domain.CommandID("admit-k10-"+string(actor)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m, Reason: "checked"})
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("%s admitted a mission with an unsettled effect: %v", actor, err)
		}
	}
	if cur, err := st.Mission(ctx, m); err != nil || cur.Execution != before.Execution {
		t.Fatalf("the refused admit changed the mission: %v %s -> %s", err, before.Execution, cur.Execution)
	}
	// Settle it the operator's way (abandon a never-dispatched intent: the
	// audit row in the same transaction), then admit succeeds.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor)
		VALUES ('e_k10', 'INTENT', 'NOT_APPLIED', 'test', 'test', 'op:eric')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE effects SET state = 'NOT_APPLIED' WHERE id = 'e_k10'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "op:eric", "admit-k10-settled", domain.Command{Kind: domain.CmdAdmitMission, Mission: m, Reason: "settled"})
}
