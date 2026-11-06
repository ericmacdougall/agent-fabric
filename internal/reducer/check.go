package reducer

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"fabric/internal/domain"
)

// Invariant names, as in the N1 probe the design adopted (EXPERIMENTS.md K).
const (
	InvStaleFence        = "stale_fence_monotonic"
	InvDuplicateWake     = "duplicate_wakeup_idempotent"
	InvCapacity          = "running_capacity"
	InvUnknownRetry      = "unknown_retry_blocked"
	InvAcceptedEvidence  = "accepted_current_live_evidence"
	InvDeliveredGen      = "delivered_current_generation"
	InvUnresolvedNoStart = "unresolved_attempt_blocks_start"
	InvRefusalNoEvents   = "refusal_emits_no_events"
)

// Violation is one failed invariant with enough context to reproduce it.
type Violation struct {
	Invariant string
	Message   string
}

func (v Violation) Error() string { return v.Invariant + ": " + v.Message }

// Check is the oracle the property tests and the store tests run after every
// transition. It lives in its own file so that a mutant of reducer.go can never
// also weaken the check. before/after are the states around one command;
// events and err are Decide's outputs.
func Check(before, after domain.Mission, cmd domain.Command, events []domain.Event, err error, env Env) error {
	// A refusal is a response, never a state change.
	if err != nil && len(events) != 0 {
		return Violation{InvRefusalNoEvents, fmt.Sprintf("refused %q but emitted %d events", err, len(events))}
	}
	if err != nil && !sameState(before, after) {
		return Violation{InvRefusalNoEvents, "refused but state changed"}
	}

	// I1: no counter regresses; a stale fence changes nothing.
	if after.Version.Regressed(before.Version) {
		return Violation{InvStaleFence, fmt.Sprintf("version regressed %+v -> %+v", before.Version, after.Version)}
	}
	if fenced(cmd.Kind) {
		if a, ok := before.Attempts[cmd.Attempt]; ok && cmd.Fence != a.Fence {
			if len(events) != 0 || !sameState(before, after) || !errors.Is(err, domain.ErrStaleFence) {
				return Violation{InvStaleFence, fmt.Sprintf("fence %d against attempt fence %d was not refused as stale (err=%v, events=%d)", cmd.Fence, a.Fence, err, len(events))}
			}
		}
	}

	// I3: capacity, per mission (R4) and fleet (R1).
	if after.LiveAttempts() > domain.MaxLiveAttempts {
		return Violation{InvCapacity, fmt.Sprintf("%d live attempts", after.LiveAttempts())}
	}
	if cmd.Kind == domain.CmdAdmitMission && env.SlotsUsed >= env.Cap && after.Execution.HoldsSlot() && !before.Execution.HoldsSlot() {
		return Violation{InvCapacity, fmt.Sprintf("admitted with %d of %d slots held", env.SlotsUsed, env.Cap)}
	}
	if !before.Execution.HoldsSlot() && after.Execution.HoldsSlot() && cmd.Kind != domain.CmdAdmitMission {
		return Violation{InvCapacity, fmt.Sprintf("%s moved %s -> %s and took a slot without admission", cmd.Kind, before.Execution, after.Execution)}
	}

	// I5: an acceptance is bound to exact current evidence.
	if acc := after.Accepted; acc != nil {
		cur := after.Version
		evd, ok := after.Evidence[domain.EvidenceKey(acc.Attempt, cur.IntentRevision, cur.CandidateGeneration, after.Head)]
		if !ok || acc.Revision != cur.IntentRevision || acc.Generation != cur.CandidateGeneration || acc.Head != after.Head ||
			evd.Revision != cur.IntentRevision || evd.Generation != cur.CandidateGeneration || evd.Head != after.Head {
			return Violation{InvAcceptedEvidence, fmt.Sprintf("accepted %+v without current evidence (evidence=%+v, version=%+v, head=%q)", *acc, evd, cur, after.Head)}
		}
	}

	// I6: a delivered generation is the current generation.
	if g := after.DeliveredGeneration; g != nil && *g != after.Version.CandidateGeneration {
		return Violation{InvDeliveredGen, fmt.Sprintf("delivered %d, current %d", *g, after.Version.CandidateGeneration)}
	}

	// I2: a second identical wake is a no-op.
	if cmd.Kind == domain.CmdWake && err == nil {
		again, aerr := Decide(after, cmd, env)
		if aerr != nil || len(again) != 0 {
			return Violation{InvDuplicateWake, fmt.Sprintf("second identical wake produced %d events, err=%v", len(again), aerr)}
		}
	}

	// I4: no resubmission while UNKNOWN.
	if cmd.Kind == domain.CmdRetryEffect {
		if f, ok := before.Effects[cmd.Effect]; ok && f.State == domain.EffectUnknown {
			if after.Effects[cmd.Effect].Submissions != f.Submissions || !errors.Is(err, domain.ErrUnknownUnresolved) {
				return Violation{InvUnknownRetry, fmt.Sprintf("retry during UNKNOWN changed submissions %d -> %d (err=%v)", f.Submissions, after.Effects[cmd.Effect].Submissions, err)}
			}
		}
	}

	// I7': an unresolved attempt blocks a new start (UNKNOWN never relaunches).
	if cmd.Kind == domain.CmdStartAttempt && before.UnresolvedAttempts() > 0 {
		for _, e := range events {
			if e.Kind == domain.EvAttemptStarted {
				return Violation{InvUnresolvedNoStart, "attempt started while another is unresolved"}
			}
		}
	}
	return nil
}

func fenced(k domain.CommandKind) bool {
	switch k {
	case domain.CmdAckStart, domain.CmdRecordEvidence, domain.CmdFinishAttempt, domain.CmdExpireLease,
		domain.CmdReconcileAttempt, domain.CmdSubmitEffect:
		return true
	}
	return false
}

// sameState compares two missions by canonical JSON, ignoring nothing: a
// refusal must leave every byte of state as it was.
func sameState(a, b domain.Mission) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// Digest is a stable fingerprint of a mission state for replay comparisons
// (encoding/json sorts map keys, so the bytes are canonical).
func Digest(m domain.Mission) string {
	b, _ := json.Marshal(m)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
