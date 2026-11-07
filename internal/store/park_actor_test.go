package store

import (
	"errors"
	"fmt"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15082 T2 (main decision): through the store, a parked mission is
// re-admitted only by an operator actor; fabd, native-runner and import are
// refused with a stored refusal and the park stays.
func TestParkedAdmitRefusedForAutomationActors(t *testing.T) {
	st, ctx := openTest(t)
	m := domain.MissionID("m_pa")
	fence := liveAttempt(t, st, ctx, m, "r_pa_1")
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		a := domain.AttemptID(fmt.Sprintf("r_pa_%d", n))
		ok(t, st, ctx, string(SchedulerActor), "fin-"+string(a), domain.Command{Kind: domain.CmdFinishAttempt, Mission: m,
			Attempt: a, Fence: fence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
		if n < domain.PrelaunchBudget {
			fence = startAttempt(t, st, ctx, m, domain.AttemptID(fmt.Sprintf("r_pa_%d", n+1)))
		}
	}
	cur, err := st.Mission(ctx, m)
	if err != nil || cur.ParkedReason == "" || cur.Execution != domain.ExecBlocked {
		t.Fatalf("setup: %v %s %q", err, cur.Execution, cur.ParkedReason)
	}
	for _, actor := range []domain.ActorID{"fabd", "native-runner", "import"} {
		_, err := st.Execute(ctx, actor, domain.CommandID("admit-"+string(actor)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m})
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Errorf("%s re-admitted a parked mission: %v", actor, err)
		}
	}
	if cur, _ := st.Mission(ctx, m); cur.ParkedReason == "" || cur.Execution != domain.ExecBlocked {
		t.Fatalf("an automated admit cleared the park: %s %q", cur.Execution, cur.ParkedReason)
	}
	ok(t, st, ctx, "chat", "admit-operator", domain.Command{Kind: domain.CmdAdmitMission, Mission: m})
	if cur, _ := st.Mission(ctx, m); cur.ParkedReason != "" || cur.Execution != domain.ExecAdmitted {
		t.Fatalf("the operator admit did not clear the park: %s %q", cur.Execution, cur.ParkedReason)
	}
}
