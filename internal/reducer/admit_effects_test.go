package reducer

import (
	"errors"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15166 (Kimi K10): ADMIT_MISSION is refused while any effect of the
// mission is unsettled, whether the broker's table (Env) or the mission's
// own state reports it, for a parked mission re-admitted by an operator and
// for an ordinary idle mission alike. Settled effects do not block.
func TestAdmitRefusedWithUnsettledEffects(t *testing.T) {
	parked := domain.Mission{ID: "m_k10p", Terminal: domain.TermNone, Execution: domain.ExecBlocked, ParkedReason: "6 consecutive attempts abandoned before launch"}
	idle := domain.Mission{ID: "m_k10i", Terminal: domain.TermNone, Execution: domain.ExecIdle}
	op := Env{Provider: domain.ProviderClaudeCode, Cap: 4, Operator: true}
	withEnv := func(n int) Env { e := op; e.UnsettledEffects = n; return e }
	withState := func(m domain.Mission, s domain.EffectState) domain.Mission {
		m.Effects = map[domain.EffectID]domain.Effect{"e_1": {ID: "e_1", Kind: "ticket_comment", State: s}}
		return m
	}
	cmd := func(m domain.Mission) domain.Command {
		return domain.Command{Kind: domain.CmdAdmitMission, Mission: m.ID, Reason: "operator checked it"}
	}
	refused := []struct {
		name string
		m    domain.Mission
		env  Env
	}{
		{"parked, broker table", parked, withEnv(1)},
		{"idle, broker table", idle, withEnv(2)},
		{"parked, mission state UNKNOWN", withState(parked, domain.EffectUnknown), op},
		{"idle, mission state QUARANTINED", withState(idle, domain.EffectQuarantined), op},
		{"idle, mission state DISPATCHED", withState(idle, domain.EffectDispatched), op},
	}
	for _, c := range refused {
		_, err := Decide(c.m, cmd(c.m), c.env)
		if !errors.Is(err, domain.ErrInvalidTransition) || !strings.Contains(err.Error(), "unsettled") {
			t.Errorf("%s: admit with an unsettled effect was not refused by name: %v", c.name, err)
		}
	}
	accepted := []struct {
		name string
		m    domain.Mission
	}{
		{"parked, none", parked},
		{"idle, none", idle},
		{"idle, settled CONFIRMED", withState(idle, domain.EffectConfirmed)},
		{"idle, settled RESOLVED_NOT_APPLIED", withState(idle, domain.EffectResolvedNotApplied)},
	}
	for _, c := range accepted {
		if evs, err := Decide(c.m, cmd(c.m), op); err != nil || len(evs) != 1 {
			t.Errorf("%s: admit with only settled effects was refused: %v %+v", c.name, err, evs)
		}
	}
}
