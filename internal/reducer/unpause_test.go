package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15085: a quarantine pauses the mission; only an operator, with a
// reason and with nothing live or unresolved, returns it to ADMITTED.
func TestUnpauseIsOperatorOnly(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdExpireLease, Mission: id, Attempt: a, Fence: fence})
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "QUARANTINED"})
	m := f.missions[id]
	if m.Execution != domain.ExecPaused {
		t.Fatalf("setup: execution %s, want PAUSED", m.Execution)
	}
	cmd := domain.Command{Kind: domain.CmdUnpauseMission, Mission: id, Reason: "runner confirmed gone"}
	if _, err := Decide(m, cmd, Env{Provider: domain.ProviderClaudeCode, Cap: 4}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("a non-operator unpaused a mission: %v", err)
	}
	if _, err := Decide(m, domain.Command{Kind: domain.CmdUnpauseMission, Mission: id}, Env{Provider: domain.ProviderClaudeCode, Cap: 4, Operator: true}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("an unpause without a reason was accepted: %v", err)
	}
	evs, err := Decide(m, cmd, Env{Provider: domain.ProviderClaudeCode, Cap: 1, SlotsUsed: 1, Operator: true})
	if err != nil || len(evs) != 1 || evs[0].Kind != domain.EvMissionUnpaused {
		t.Fatalf("operator unpause: %v %+v", err, evs)
	}
	after := Apply(m, evs[0])
	if after.Execution != domain.ExecAdmitted || after.Attempts[a].State != domain.AttemptQuarantined {
		t.Errorf("after unpause: execution %s, attempt %s; want ADMITTED, QUARANTINED", after.Execution, after.Attempts[a].State)
	}
	if _, err := Decide(after, cmd, Env{Provider: domain.ProviderClaudeCode, Cap: 4, Operator: true}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("an admitted mission was unpaused: %v", err)
	}
	if _, err := Decide(after, domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "writer"}, Env{Provider: domain.ProviderClaudeCode, Cap: 4}); err != nil {
		t.Errorf("an unpaused mission could not start: %v", err)
	}
}
