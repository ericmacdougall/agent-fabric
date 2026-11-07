package reducer

import (
	"errors"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// deliveredMission is reviewed, accepted and delivered (deployed) at head
// sha_a, generation gen.
func deliveredMission(t *testing.T) (*fleet, domain.MissionID, int64) {
	t.Helper()
	f, id, a, fence := startedMission(t)
	reviewed(t, f, id, a, fence, "rcpt")
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	gen := f.missions[id].Version.CandidateGeneration
	mustStep(t, f, domain.Command{Kind: domain.CmdDeliver, Mission: id, Attempt: a, Generation: gen})
	return f, id, gen
}

func liveTest(id domain.MissionID, gen int64, head string) domain.Command {
	return domain.Command{Kind: domain.CmdRecordLiveTest, Mission: id, Generation: gen, Head: head,
		Host: "h_t2", Target: "https://staging.example.test", Receipt: "rcpt_live"}
}

// AC-6 / NEG-6: the old audit counted a merged (here: delivered) mission as
// done. Done is refused until a live_test receipt names the deployed commit,
// and the refusal names what is missing.
func TestDoneNeedsLiveTestForDeployedCommit(t *testing.T) {
	f, id, gen := deliveredMission(t)
	done := domain.Command{Kind: domain.CmdCompleteMission, Mission: id}
	_, err := f.step(t, done)
	if !errors.Is(err, domain.ErrLiveTestMissing) || !strings.Contains(err.Error(), "sha_a") {
		t.Fatalf("done on a delivered mission without a live test: %v, want ErrLiveTestMissing naming sha_a", err)
	}
	// Live tests that do not bind to the deployed commit are refused.
	other := liveTest(id, gen, "sha_other")
	if _, err := f.step(t, other); !errors.Is(err, domain.ErrEvidenceStale) {
		t.Errorf("live test of another commit: %v, want ErrEvidenceStale", err)
	}
	old := liveTest(id, gen-1, "sha_a")
	if _, err := f.step(t, old); !errors.Is(err, domain.ErrGenerationStale) {
		t.Errorf("live test for an old generation: %v, want ErrGenerationStale", err)
	}
	vac := liveTest(id, gen, "sha_a")
	vac.Vacuous = true
	if _, err := f.step(t, vac); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("vacuous live test: %v, want ErrInvalidTransition", err)
	}
	noTarget := liveTest(id, gen, "sha_a")
	noTarget.Target = ""
	if _, err := f.step(t, noTarget); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("live test naming no target: %v, want ErrInvalidTransition", err)
	}
	if _, err := f.step(t, done); !errors.Is(err, domain.ErrLiveTestMissing) {
		t.Fatalf("done after refused live tests: %v, want ErrLiveTestMissing", err)
	}
	// The real one: LIVE_VERIFIED, then done.
	mustStep(t, f, liveTest(id, gen, "sha_a"))
	m := f.missions[id]
	if m.Delivery != domain.DelivLiveVerified || m.LiveTest == nil || m.LiveTest.Head != "sha_a" || m.LiveTest.Target == "" {
		t.Fatalf("after the live test: delivery %s, live test %+v", m.Delivery, m.LiveTest)
	}
	mustStep(t, f, done)
	if m := f.missions[id]; m.Terminal != domain.TermSucceeded || m.Execution.HoldsSlot() {
		t.Fatalf("after done: terminal %s, execution %s", m.Terminal, m.Execution)
	}
}

// A mission that is only accepted (merged, not deployed) cannot record a live
// test or be done; a new candidate after a live test clears it.
func TestLiveTestNeedsDeployAndCurrentCandidate(t *testing.T) {
	f, id, a, fence := startedMission(t)
	reviewed(t, f, id, a, fence, "rcpt")
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	gen := f.missions[id].Version.CandidateGeneration
	if _, err := f.step(t, liveTest(id, gen, "sha_a")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("live test before deploy: %v, want ErrInvalidTransition", err)
	}
	if _, err := f.step(t, domain.Command{Kind: domain.CmdCompleteMission, Mission: id}); !errors.Is(err, domain.ErrLiveTestMissing) {
		t.Errorf("done on an accepted, undeployed mission: %v, want ErrLiveTestMissing", err)
	}
	f2, id2, gen2 := deliveredMission(t)
	mustStep(t, f2, liveTest(id2, gen2, "sha_a"))
	mustStep(t, f2, domain.Command{Kind: domain.CmdSelectCandidate, Mission: id2, Head: "sha_b"})
	if m := f2.missions[id2]; m.LiveTest != nil || m.Delivery == domain.DelivLiveVerified {
		t.Fatalf("a live test survived a new candidate: %+v %s", m.LiveTest, m.Delivery)
	}
	if _, err := f2.step(t, domain.Command{Kind: domain.CmdCompleteMission, Mission: id2}); !errors.Is(err, domain.ErrLiveTestMissing) {
		t.Errorf("done after a new candidate: %v, want ErrLiveTestMissing", err)
	}
}

// Done waits for live attempts (a reviewer still running) to end.
func TestDoneRefusedWhileAttemptLive(t *testing.T) {
	f, id, gen := deliveredMission(t)
	mustStep(t, f, liveTest(id, gen, "sha_a"))
	mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_9", Host: "h_t2", Role: "reviewer"})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdCompleteMission, Mission: id}); !errors.Is(err, domain.ErrAttemptLive) {
		t.Fatalf("done with a live attempt: %v, want ErrAttemptLive", err)
	}
}

// Review M1 on fc8e035: done with an unsettled effect would strand it, since
// a terminal mission refuses the RESOLVE_EFFECT that settles it.
func TestDoneRefusedWhileEffectUnsettled(t *testing.T) {
	f, id, a, fence := startedMission(t)
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdSubmitEffect, Mission: id, Attempt: a, Fence: fence, EffectKind: "ticket_comment",
		Request: []byte(`{"ticket": 14922, "body": "x"}`)})
	eid := ev[0].Effect
	mustStep(t, f, domain.Command{Kind: domain.CmdEffectUnknown, Mission: id, Effect: eid, Submission: 1})
	reviewed(t, f, id, a, fence, "rcpt")
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	gen := f.missions[id].Version.CandidateGeneration
	mustStep(t, f, domain.Command{Kind: domain.CmdDeliver, Mission: id, Attempt: a, Generation: gen})
	mustStep(t, f, liveTest(id, gen, "sha_a"))
	done := domain.Command{Kind: domain.CmdCompleteMission, Mission: id}
	if _, err := f.step(t, done); !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("done with an UNKNOWN effect: %v, want ErrUnknownUnresolved", err)
	}
	// Control: once the effect is settled, done goes through.
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "CONFIRMED"})
	mustStep(t, f, done)
}

// Review M2 on fc8e035: a revised intent revokes the acceptance, and with it
// the delivery and live test it rested on; a re-acceptance at the same head
// needs a new delivery and a new live test.
func TestRevisedIntentClearsLiveTest(t *testing.T) {
	f, id, gen := deliveredMission(t)
	mustStep(t, f, liveTest(id, gen, "sha_a"))
	mustStep(t, f, domain.Command{Kind: domain.CmdReviseIntent, Mission: id, Reason: "new scope"})
	m := f.missions[id]
	if m.LiveTest != nil || m.Delivery == domain.DelivLiveVerified || m.Delivery == domain.DelivDelivered || m.DeliveredGeneration != nil {
		t.Fatalf("after a revised intent: live test %+v, delivery %s, delivered %v", m.LiveTest, m.Delivery, m.DeliveredGeneration)
	}
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Mission: id, Attempt: "r_2", Host: "h_t2", Role: "writer"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAckStart, Mission: id, Attempt: "r_2", Fence: ev[0].Fence})
	reviewed(t, f, id, "r_2", ev[0].Fence, "rcpt_2")
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: "r_2"})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdCompleteMission, Mission: id}); !errors.Is(err, domain.ErrLiveTestMissing) {
		t.Fatalf("done after re-acceptance on the old live test: %v, want ErrLiveTestMissing", err)
	}
}

// Review N2 on aecf3fa: a QUARANTINED effect has an operator exit, with a
// reason and an evidence receipt, to a settled terminal state.
func TestQuarantinedEffectOperatorResolve(t *testing.T) {
	f, id, a, fence := startedMission(t)
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdSubmitEffect, Mission: id, Attempt: a, Fence: fence, EffectKind: "ticket_comment",
		Request: []byte(`{"ticket": 14922, "body": "q"}`)})
	eid := ev[0].Effect
	mustStep(t, f, domain.Command{Kind: domain.CmdEffectUnknown, Mission: id, Effect: eid, Submission: 1})
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: "QUARANTINED"})
	res := domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: eid, Submission: 1, Result: string(domain.EffectResolvedNotApplied)}
	for _, c := range []domain.Command{
		func() domain.Command { c := res; c.Receipt = "rcpt_op"; return c }(),  // no reason
		func() domain.Command { c := res; c.Reason = "checked"; return c }(),   // no evidence
		func() domain.Command { c := res; c.Result = "CONFIRMED"; return c }(), // not an operator outcome
	} {
		if _, err := f.step(t, c); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("operator resolve %+v: %v, want ErrInvalidTransition", c.Result, err)
		}
	}
	res.Reason, res.Receipt = "ticket comments read back: not posted", "rcpt_op"
	mustStep(t, f, res)
	if got := f.missions[id].Effects[eid].State; got != domain.EffectResolvedNotApplied {
		t.Fatalf("after the operator resolve: %s", got)
	}
	if n := f.missions[id].UnsettledEffects(); n != 0 {
		t.Fatalf("a resolved effect still counts as unsettled (%d)", n)
	}
}
