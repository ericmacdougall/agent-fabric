package store

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// Red-team N2 on b37056a, through the store: the unsettled-effect count is
// computed for UNPAUSE_MISSION, so a quarantined mission with an open
// effect cannot be unpaused even by an operator.
func TestUnpauseCountsUnsettledEffects(t *testing.T) {
	st, ctx := openTest(t)
	m := domain.MissionID("m_ue")
	f := liveAttempt(t, st, ctx, m, "r_ue_1")
	ok(t, st, ctx, "fabd", "ack:r_ue_1", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: "r_ue_1", Fence: f})
	if _, err := st.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_ue', 'm_ue', 'r_ue_1', $1, 'ticket_comment', 'e_ue', 'e_ue', '{}', 'sha', 'INTENT')`, f); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE attempts SET lease_until = now() - interval '1 minute' WHERE id = 'r_ue_1'`); err != nil {
		t.Fatal(err)
	}
	ok(t, st, ctx, "fabd", "exp-r_ue_1", domain.Command{Kind: domain.CmdExpireLease, Mission: m, Attempt: "r_ue_1", Fence: f})
	ok(t, st, ctx, "fabd", "q-r_ue_1", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: m, Attempt: "r_ue_1", Fence: f, Result: "QUARANTINED"})
	if cur, err := st.Mission(ctx, m); err != nil || cur.Execution != domain.ExecPaused {
		t.Fatalf("setup: %v %s, want PAUSED", err, cur.Execution)
	}
	_, err := st.Execute(ctx, "op:eric", "unpause-ue", domain.Command{Kind: domain.CmdUnpauseMission, Mission: m, Reason: "checked"})
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("an operator unpaused a mission with an unsettled effect: %v", err)
	}
	if cur, _ := st.Mission(ctx, m); cur.Execution != domain.ExecPaused {
		t.Fatalf("the refused unpause changed the mission: %s", cur.Execution)
	}
}
