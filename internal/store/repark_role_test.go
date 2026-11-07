package store

import (
	"fmt"
	"testing"

	"fabric/internal/domain"
)

// Red-team nit 2 on 5fa185c: the re-park (a scheduler-reserved
// MISSION_PARKED, migrations 0015 and 0017) is written under the app role
// that fabd and fab use in production, not only under the owner.
func TestReparkUnderAppRole(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	app := openAs(t, ctx, r.App())
	m := domain.MissionID("m_rp")
	fence := liveAttempt(t, app, ctx, m, "r_rp_1")
	rev := ok(t, app, ctx, "fabd", "start:r_rp_rev", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: "r_rp_rev", Host: "h_t2", Role: "reviewer"})
	rfence := rev.Events[0].Fence
	ok(t, app, ctx, "fabd", "ack:r_rp_rev", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: "r_rp_rev", Fence: rfence})
	for n := 1; n <= domain.PrelaunchBudget; n++ {
		a := domain.AttemptID(fmt.Sprintf("r_rp_%d", n))
		ok(t, app, ctx, string(SchedulerActor), "fin-"+string(a), domain.Command{Kind: domain.CmdFinishAttempt, Mission: m,
			Attempt: a, Fence: fence, Outcome: "abandoned", Prelaunch: true, Reason: "dispatch denied"})
		if n < domain.PrelaunchBudget {
			fence = startAttempt(t, app, ctx, m, domain.AttemptID(fmt.Sprintf("r_rp_%d", n+1)))
		}
	}
	ok(t, app, ctx, "fabd", "fin-r_rp_rev", domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: "r_rp_rev", Fence: rfence, Outcome: "checkpoint"})
	resp := ok(t, app, ctx, "fabd", "verify-r_rp_rev", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: m, Attempt: "r_rp_rev", Fence: rfence, Result: "VERIFIED"})
	if n := len(resp.Events); n != 2 || resp.Events[1].Kind != domain.EvMissionParked {
		t.Fatalf("reconcile under the app role: %d events %+v, want RECONCILED then MISSION_PARKED", n, resp.Events)
	}
	if cur, err := app.Mission(ctx, m); err != nil || cur.Execution != domain.ExecBlocked || cur.ParkedReason == "" {
		t.Fatalf("after the re-park: %v %s %q, want BLOCKED and parked", err, cur.Execution, cur.ParkedReason)
	}
}
