package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

func TestParkedAdmitRefusedWithUnsettledEffects(t *testing.T) {
	for _, execution := range []domain.ExecutionState{domain.ExecBlocked, domain.ExecAdmitted} {
		t.Run(string(execution), func(t *testing.T) {
			m := domain.Mission{
				ID:           "m_parked_effects",
				Terminal:     domain.TermNone,
				Execution:    execution,
				ParkedReason: "operator review required",
			}
			cmd := domain.Command{Kind: domain.CmdAdmitMission, Mission: m.ID, Reason: "checked"}
			if _, err := Decide(m, cmd, Env{Cap: 4, Operator: true, UnsettledEffects: 1}); !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("parked admission with an unsettled effect was accepted: %v", err)
			}
			events, err := Decide(m, cmd, Env{Cap: 4, Operator: true})
			if err != nil || len(events) != 1 || events[0].Kind != domain.EvMissionAdmitted {
				t.Fatalf("settled parked admission: %+v %v", events, err)
			}
		})
	}
}
