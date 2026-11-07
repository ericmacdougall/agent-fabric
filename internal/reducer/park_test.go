package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15082 T2: a park reached while another attempt is live is not lost.
// No new attempt may start, and when the last live attempt ends the mission
// becomes BLOCKED with the park's reason and count kept.
func TestParkHeldWhileAnotherAttemptLive(t *testing.T) {
	f, id, writer, wfence := startedMission(t)
	rev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_rev", Host: "h_t2", Role: "reviewer"})
	rfence := rev[0].Fence
	mustStep(t, f, domain.Command{Kind: domain.CmdAckStart, Mission: id, Attempt: "r_rev", Fence: rfence})
	var last []domain.Event
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		last = mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: writer, Fence: wfence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
		if n < domain.PrelaunchBudget {
			writer = domain.AttemptID("r_w" + string(rune('a'+n)))
			ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: writer, Host: "h_t2", Role: "writer"})
			wfence = ev[0].Fence
		}
	}
	if last[len(last)-1].Kind != domain.EvMissionParked {
		t.Fatalf("no park after %d failures: %+v", domain.PrelaunchBudget, last)
	}
	parked := f.missions[id]
	if parked.ParkedReason == "" {
		t.Fatal("park reason not recorded")
	}
	_, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_after", Host: "h_t2", Role: "writer"})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("a writer started on a parked mission while the reviewer was live: %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: "r_rev", Fence: rfence, Outcome: "checkpoint"})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_after2", Host: "h_t2", Role: "writer"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("a writer started on a parked mission after the reviewer finished: %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: "r_rev", Fence: rfence, Result: "VERIFIED"})
	m := f.missions[id]
	if m.Execution != domain.ExecBlocked || m.ParkedReason != parked.ParkedReason || m.PrelaunchFailures != parked.PrelaunchFailures {
		t.Errorf("after the reviewer ended: execution %s reason %q failures %d; want BLOCKED %q %d",
			m.Execution, m.ParkedReason, m.PrelaunchFailures, parked.ParkedReason, parked.PrelaunchFailures)
	}
}

// goodqa#15082 T2 review B1/B2: a log written before the fix (a park while
// the reviewer was live, then the reviewer's finish and reconcile with no
// re-park) replays exactly as it always did, and the wedged mission it
// leaves (parked, slot held, nothing live) can be re-admitted at the cap.
func TestOldParkLogReplaysAndCanBeReadmitted(t *testing.T) {
	f, id, _, _ := startedMission(t)
	m := f.missions[id]
	rev := domain.Event{Kind: domain.EvAttemptStarted, Mission: id, Attempt: "r_rev", Fence: m.Version.AttemptFence + 1, Role: "reviewer", Host: "h_t2", AttemptTo: domain.AttemptLeased}
	old := []domain.Event{
		rev,
		{Kind: domain.EvAttemptAcked, Mission: id, Attempt: "r_rev", AttemptTo: domain.AttemptRunning},
		{Kind: domain.EvAttemptFinished, Mission: id, Attempt: "r_1", Outcome: "abandoned", Prelaunch: true, AttemptTo: domain.AttemptFailedDefinite},
		{Kind: domain.EvMissionParked, Mission: id, Count: domain.PrelaunchBudget, Reason: "parked"},
		{Kind: domain.EvAttemptFinished, Mission: id, Attempt: "r_rev", Outcome: "checkpoint", AttemptTo: domain.AttemptSucceededUnverified},
		{Kind: domain.EvAttemptReconciled, Mission: id, Attempt: "r_rev", Result: "VERIFIED", AttemptTo: domain.AttemptVerified},
	}
	for _, e := range old {
		m = Apply(m, e)
	}
	// The pre-fix projection: slot held, counts reset, park reason kept.
	if m.Execution != domain.ExecAdmitted || m.PrelaunchFailures != 0 || m.ParkedReason != "parked" {
		t.Fatalf("old log replayed differently: execution %s failures %d reason %q", m.Execution, m.PrelaunchFailures, m.ParkedReason)
	}
	if _, err := Decide(m, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_x", Host: "h_t2", Role: "writer"}, Env{Cap: 4}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("a wedged parked mission started an attempt: %v", err)
	}
	// Main decision: automation (fabd, native-runner, import) never clears
	// a park; only an operator admit does.
	if _, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: id}, Env{Cap: 4}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("a non-operator admit of a parked mission was accepted: %v", err)
	}
	if _, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: id}, Env{Cap: 1, SlotsUsed: 1, Operator: true}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("an operator admit of a parked mission without a reason was accepted: %v", err)
	}
	evs, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: id, Reason: "looked at it"}, Env{Cap: 1, SlotsUsed: 1, Operator: true})
	if err != nil {
		t.Fatalf("re-admission of a wedged parked mission refused: %v", err)
	}
	for _, e := range evs {
		m = Apply(m, e)
	}
	if m.Execution != domain.ExecAdmitted || m.ParkedReason != "" {
		t.Errorf("re-admission did not clear the park: %s %q", m.Execution, m.ParkedReason)
	}
	if _, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: id}, Env{Cap: 4, Operator: true}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("an unparked admitted mission was admitted again: %v", err)
	}
}

// Main decision (goodqa#15082 T2): a BLOCKED parked mission is re-admitted
// only by an operator; fabd, the native runner and the importer are refused.
func TestParkedAdmitIsOperatorOnly(t *testing.T) {
	for _, a := range []struct {
		actor domain.ActorID
		want  bool
	}{{"chat", false}, {"op:eric", true}, {"fabd", false}, {"native-runner", false}, {"import", false}, {"asks", false}, {"op:", false}, {"chatbot", false}} {
		if got := domain.IsOperator(a.actor); got != a.want {
			t.Errorf("IsOperator(%q) = %v, want %v", a.actor, got, a.want)
		}
	}
	m := domain.Mission{ID: "m_po", Terminal: domain.TermNone, Execution: domain.ExecBlocked, ParkedReason: "6 consecutive attempts abandoned before launch"}
	if _, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_po"}, Env{Cap: 4}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("an automated admit cleared a park: %v", err)
	}
	evs, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_po", Reason: "looked at it"}, Env{Cap: 4, Operator: true})
	if err != nil || len(evs) != 1 || Apply(m, evs[0]).ParkedReason != "" {
		t.Errorf("an operator admit did not clear the park: %v %+v", err, evs)
	}
	m.ParkedReason = ""
	if _, err := Decide(m, domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_po"}, Env{Cap: 4}); err != nil {
		t.Errorf("an automated admit of an unparked BLOCKED mission was refused: %v", err)
	}
}
