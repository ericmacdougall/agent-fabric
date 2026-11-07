package reducer

import (
	"testing"

	"fabric/internal/domain"
)

// Red-team F1: a pre-launch abandonment backs the mission off by a pure
// function of the count, a run of them parks it, and a launched runner or a
// re-admission resets the count.
func TestPrelaunchBackoffAndPark(t *testing.T) {
	f, id, a, fence := startedMission(t)
	var last []domain.Event
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		last = mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
		m := f.missions[id]
		if n < domain.PrelaunchBudget {
			if len(last) != 2 || last[1].Kind != domain.EvDispatchDeferred || last[1].BackoffMS != domain.PrelaunchBackoffMS(n) || m.PrelaunchFailures != n {
				t.Fatalf("failure %d: events %+v, count %d", n, last, m.PrelaunchFailures)
			}
			mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: domain.AttemptID("r_" + string(rune('a'+n))), Host: "h_t2", Role: "writer"})
			a = domain.AttemptID("r_" + string(rune('a'+n)))
			fence = f.missions[id].Version.AttemptFence
		}
	}
	m := f.missions[id]
	if last[len(last)-1].Kind != domain.EvMissionParked || m.Execution != domain.ExecBlocked || m.ParkedReason == "" {
		t.Fatalf("after %d: %+v, execution %s", domain.PrelaunchBudget, last, m.Execution)
	}
	if _, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_after", Host: "h_t2", Role: "writer"}); err == nil {
		t.Fatal("a parked mission started an attempt")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdAdmitMission, Mission: id})
	if m := f.missions[id]; m.PrelaunchFailures != 0 || m.ParkedReason != "" || m.Execution != domain.ExecAdmitted {
		t.Fatalf("re-admission did not reset: %+v", m)
	}
}

func TestLaunchedFinishResetsPrelaunchCount(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "abandoned", Prelaunch: true, Reason: "hold refused"})
	mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "writer"})
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: "r_2", Fence: f.missions[id].Version.AttemptFence, Outcome: "abandoned", Reason: "no structured output"})
	if m := f.missions[id]; m.PrelaunchFailures != 0 || m.DeferMS != 0 {
		t.Fatalf("a finish from a launched runner kept the count: %+v", m)
	}
	if _, err := f.step(t, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: "r_2", Fence: 2, Outcome: "checkpoint", Prelaunch: true}); err == nil {
		t.Fatal("prelaunch on a non-abandon outcome was accepted")
	}
	if got := domain.PrelaunchBackoffMS(20); got != domain.PrelaunchMaxMS {
		t.Fatalf("backoff is not capped: %d", got)
	}
}

// Kimi rev 7 (pending park), checked against this tree: the budget runs out
// on one role while another role's attempt is live. The park waits for it,
// and that attempt's launched finish neither erases the run nor re-admits.
func TestPendingParkCompletesAfterOtherAttemptEnds(t *testing.T) {
	f, id, writer, wfence := startedMission(t)
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		r := domain.AttemptID("r_rev" + string(rune('a'+n)))
		ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: r, Host: "h_t2", Role: "reviewer"})
		mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: r, Fence: ev[0].Fence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
	}
	m := f.missions[id]
	if m.ParkedReason == "" || m.PrelaunchFailures != domain.PrelaunchBudget {
		t.Fatalf("budget spent but not parked: %+v", m)
	}
	if !m.Execution.HoldsSlot() {
		t.Fatalf("the park freed the slot while %s is live: %s", writer, m.Execution)
	}
	if _, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_more", Host: "h_t2", Role: "reviewer"}); err == nil {
		t.Fatal("an attempt started while the park is pending")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: writer, Fence: wfence, Outcome: "abandoned", Reason: "runner exited"})
	m = f.missions[id]
	if m.Execution != domain.ExecBlocked || m.ParkedReason == "" || m.PrelaunchFailures != domain.PrelaunchBudget || m.WaitKey != "" {
		t.Fatalf("after the live attempt ended: execution %s, reason %q, count %d", m.Execution, m.ParkedReason, m.PrelaunchFailures)
	}
}
