package reducer

// Failing-first tests ported from the 8f83662f review reproducers (RV-1,
// RV-4, RV-5, RV-6, RV-7, RV-17). Each one failed against 8f83662f and passes
// with the fix; the reviewer's originals passed against the defect.

import (
	"encoding/json"
	"errors"
	"testing"

	"fabric/internal/domain"
)

// reviewed walks a mission through the review path: current evidence,
// ready_for_review with a claim, reconcile VERIFIED. Returns the attempt.
func reviewed(t *testing.T, f *fleet, id domain.MissionID, a domain.AttemptID, fence int64, receipt string) {
	t.Helper()
	m := f.missions[id]
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration, Head: m.Head, Receipt: receipt})
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: receipt}}})
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "VERIFIED"})
}

// RV-1 (H6): a second START with the same role is refused; a reviewer may run
// beside the writer and both stay addressable; RELEASE is reachable after both end.
func TestRV1_LiveAttemptsStayAddressable(t *testing.T) {
	f, id, a, fence := startedMission(t)
	if _, err := f.step(t, domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: id, Attempt: "r_w2", Host: "h_t2", Role: "writer"}); !errors.Is(err, domain.ErrAttemptLive) {
		t.Fatalf("second writer: want ErrAttemptLive, got %v", err)
	}
	ev := mustStep(t, f, domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: id, Attempt: "r_rev", Host: "h_t2", Role: "reviewer"})
	f2 := ev[0].Fence
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "checkpoint"})
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "VERIFIED"})
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: "r_rev", Fence: f2, Outcome: "checkpoint"})
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: "r_rev", Fence: f2, Result: "VERIFIED"})
	if f.missions[id].LiveAttempts() != 0 {
		t.Fatal("attempts still live")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdRelease, Mission: id})
	if f.missions[id].Execution != domain.ExecIdle {
		t.Fatalf("release unreachable: %s", f.missions[id].Execution)
	}
}

// RV-4 (H7): an identical request while its twin is UNKNOWN or DISPATCHED is
// refused; after resolution it is a new occurrence.
func TestRV4_NoResubmitWhileTwinUnresolved(t *testing.T) {
	f, id, a, fence := startedMission(t)
	req := json.RawMessage(`{"ticket": 14791, "body": "checkpoint"}`)
	sub := domain.Command{Kind: domain.CmdSubmitEffect, Mission: id, Attempt: a, Fence: fence, EffectKind: "ticket_comment", Request: req}
	ev := mustStep(t, f, sub)
	e1 := ev[0].Effect
	if _, err := f.step(t, sub); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("resubmit while DISPATCHED: want ErrDuplicate, got %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdEffectUnknown, Mission: id, Effect: e1, Submission: 1, Reason: "timeout"})
	if _, err := f.step(t, sub); !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("resubmit while UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdResolveEffect, Mission: id, Effect: e1, Submission: 1, Result: "CONFIRMED"})
	ev = mustStep(t, f, sub)
	if ev[0].Occurrence != 2 {
		t.Fatalf("after resolution: want occurrence 2, got %d", ev[0].Occurrence)
	}
}

// RV-5 (H8): RELEASE is refused while an attempt is UNKNOWN; the slot stays held.
func TestRV5_ReleaseRefusedWhileUnknown(t *testing.T) {
	f, id, a, fence := startedMission(t)
	mustStep(t, f, domain.Command{Kind: domain.CmdExpireLease, Mission: id, Attempt: a, Fence: fence})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdRelease, Mission: id}); !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("release while UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	if !f.missions[id].Execution.HoldsSlot() {
		t.Fatal("slot released while an attempt is UNKNOWN")
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "FAILED_DEFINITE"})
	mustStep(t, f, domain.Command{Kind: domain.CmdRelease, Mission: id})
}

// RV-6 (H9): ACCEPT is refused from a RUNNING attempt and from an UNVERIFIED
// candidate; it succeeds only after the review path.
func TestRV6_AcceptOnlyAfterReview(t *testing.T) {
	f, id, a, fence := startedMission(t)
	m := f.missions[id]
	mustStep(t, f, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision, Generation: m.Version.CandidateGeneration, Head: m.Head, Receipt: "rcpt_lint"})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("accept from RUNNING/UNVERIFIED: want ErrInvalidTransition, got %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdFinishAttempt, Mission: id, Attempt: a, Fence: fence, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rcpt_lint"}}})
	if _, err := f.step(t, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("accept before the attempt is VERIFIED: want ErrInvalidTransition, got %v", err)
	}
	mustStep(t, f, domain.Command{Kind: domain.CmdReconcileAttempt, Mission: id, Attempt: a, Fence: fence, Result: "VERIFIED"})
	mustStep(t, f, domain.Command{Kind: domain.CmdAccept, Mission: id, Attempt: a})
	if f.missions[id].Candidate != domain.CandAccepted {
		t.Fatal("not accepted after review")
	}
}

// RV-7: evidence for a subject that does not exist yet is refused.
func TestRV7_FutureSubjectEvidenceRefused(t *testing.T) {
	f, id, a, fence := startedMission(t)
	m := f.missions[id]
	_, err := f.step(t, domain.Command{Kind: domain.CmdRecordEvidence, Mission: id, Attempt: a, Fence: fence,
		Revision: m.Version.IntentRevision + 1, Generation: m.Version.CandidateGeneration + 1, Head: m.Head, Receipt: "rcpt_future"})
	if !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("future subject: want ErrEvidenceStale, got %v", err)
	}
}

// RV-17: two different subjects never share an evidence key.
func TestRV17_EvidenceKeyNoCollision(t *testing.T) {
	if domain.EvidenceKey("r_1", 0, 1, "h", "x|y") == domain.EvidenceKey("r_1", 0, 1, "h|x", "y") {
		t.Fatal("delimiter collision")
	}
}
