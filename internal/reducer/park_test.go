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
