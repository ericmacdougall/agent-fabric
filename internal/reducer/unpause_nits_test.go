package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// Red-team N1 on b37056a: parked while the reviewer ran, then the reviewer
// quarantined (PAUSED, park kept). The operator unpause must not leave an
// admitted, parked, idle mission holding a slot: it is re-parked to BLOCKED.
func TestUnpauseOfParkedMissionFreesTheSlot(t *testing.T) {
	f, id, writer, wfence := startedMission(t)
	rev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_rev", Host: "h_t2", Role: "reviewer"})
	rfence := rev[0].Fence
	mustStep(t, f, domain.Command{Kind: domain.CmdAckStart, Mission: id, Attempt: "r_rev", Fence: rfence})
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: writer, Fence: wfence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
		if n < domain.PrelaunchBudget {
			writer = domain.AttemptID("r_w" + string(rune('a'+n)))
			wfence = mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: writer, Host: "h_t2", Role: "writer"})[0].Fence
		}
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdExpireLease, Mission: id, Attempt: "r_rev", Fence: rfence})
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: "r_rev", Fence: rfence, Result: "QUARANTINED"})
	if m := f.missions[id]; m.Execution != domain.ExecPaused || m.ParkedReason == "" {
		t.Fatalf("setup: %s %q, want PAUSED and parked", m.Execution, m.ParkedReason)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdUnpauseMission, Mission: id, Reason: "reviewer runner confirmed gone"})
	m := f.missions[id]
	if m.Execution != domain.ExecBlocked || m.ParkedReason == "" {
		t.Errorf("after unpause: %s %q; want BLOCKED with the park kept", m.Execution, m.ParkedReason)
	}
}

// Red-team N2 on b37056a: unpause is refused while the mission has
// unsettled broker effects (the quarantined effect's outcome is unknown).
func TestUnpauseRefusedWithUnsettledEffects(t *testing.T) {
	m := domain.Mission{ID: "m_ue", Terminal: domain.TermNone, Execution: domain.ExecPaused}
	cmd := domain.Command{Kind: domain.CmdUnpauseMission, Mission: "m_ue", Reason: "checked"}
	if _, err := Decide(m, cmd, Env{Cap: 4, Operator: true, UnsettledEffects: 1}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("unpause with an unsettled effect was accepted: %v", err)
	}
	if _, err := Decide(m, cmd, Env{Cap: 4, Operator: true}); err != nil {
		t.Errorf("unpause with no unsettled effects was refused: %v", err)
	}
}
