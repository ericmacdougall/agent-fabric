package reducer

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"fabric/internal/domain"
)

// Invariant names, as in the N1 probe the design adopted (EXPERIMENTS.md K)
// plus the ones the 8f83662f review added (H6-H9).
const (
	InvStaleFence        = "stale_fence_monotonic"
	InvDuplicateWake     = "duplicate_wakeup_idempotent"
	InvCapacity          = "running_capacity"
	InvUnknownRetry      = "unknown_retry_blocked"
	InvAcceptedEvidence  = "accepted_current_live_evidence"
	InvDeliveredGen      = "delivered_current_generation"
	InvUnresolvedNoStart = "unresolved_attempt_blocks_start"
	InvRefusalNoEvents   = "refusal_emits_no_events"
	InvArbitration       = "review_needs_current_receipts"   // H3
	InvEffectKey         = "effect_key_canonical_occurrence" // H1
	InvAddressable       = "live_attempt_addressable"        // review H6
	InvSlotHeld          = "live_or_unresolved_holds_slot"   // review H8
	InvAcceptReviewed    = "accept_only_after_review"        // review H9
)

// Violation is one failed invariant with enough context to reproduce it.
type Violation struct {
	Invariant string
	Message   string
}

func (v Violation) Error() string { return v.Invariant + ": " + v.Message }

// The oracle computes everything it checks with its own loops: it never calls
// the domain helpers the reducer uses (CurrentEvidence, Occurrences,
// LiveAttempts, ...), so a mutant in a shared helper cannot blind it.

func liveCount(m domain.Mission) (live, unresolved int, roles map[string]int) {
	roles = map[string]int{}
	for _, a := range m.Attempts {
		switch a.State {
		case domain.AttemptAdmitted, domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning:
			live++
			roles[a.Role]++
		case domain.AttemptUnknown, domain.AttemptReconciling, domain.AttemptSucceededUnverified:
			unresolved++
		}
	}
	return
}

func holdsSlot(e domain.ExecutionState) bool {
	return e == domain.ExecAdmitted || e == domain.ExecRunning || e == domain.ExecWaiting || e == domain.ExecPaused
}

func hasCurrentEvidence(m domain.Mission, attempt domain.AttemptID) bool {
	for _, e := range m.Evidence {
		if e.Attempt == attempt && !e.Vacuous && e.Revision == m.Version.IntentRevision &&
			e.Generation == m.Version.CandidateGeneration && e.Head == m.Head {
			return true
		}
	}
	return false
}

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

	// I1: no counter regresses; a wrong fence changes nothing.
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

	live, unresolved, roles := liveCount(after)

	// I3: capacity, per mission (R4: one live attempt per role) and fleet (R1).
	if live > domain.MaxLiveAttempts {
		return Violation{InvCapacity, fmt.Sprintf("%d live attempts", live)}
	}
	for role, n := range roles {
		if n > 1 {
			return Violation{InvCapacity, fmt.Sprintf("%d live attempts with role %q", n, role)}
		}
	}
	if cmd.Kind == domain.CmdAdmitMission && env.SlotsUsed >= env.Cap && holdsSlot(after.Execution) && !holdsSlot(before.Execution) {
		return Violation{InvCapacity, fmt.Sprintf("admitted with %d of %d slots held", env.SlotsUsed, env.Cap)}
	}
	if !holdsSlot(before.Execution) && holdsSlot(after.Execution) && cmd.Kind != domain.CmdAdmitMission {
		return Violation{InvCapacity, fmt.Sprintf("%s moved %s -> %s and took a slot without admission", cmd.Kind, before.Execution, after.Execution)}
	}

	// H8: a live or unresolved attempt means the mission still owns its slot.
	if (live > 0 || unresolved > 0) && !holdsSlot(after.Execution) {
		return Violation{InvSlotHeld, fmt.Sprintf("execution %s with %d live and %d unresolved attempts", after.Execution, live, unresolved)}
	}

	// H6: every live attempt is addressable: its fence is unique among live
	// attempts and not above the mission counter.
	seen := map[int64]domain.AttemptID{}
	for _, a := range after.Attempts {
		switch a.State {
		case domain.AttemptAdmitted, domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning:
			if a.Fence > after.Version.AttemptFence || a.Fence <= 0 {
				return Violation{InvAddressable, fmt.Sprintf("%s holds fence %d, mission at %d", a.ID, a.Fence, after.Version.AttemptFence)}
			}
			if other, dup := seen[a.Fence]; dup {
				return Violation{InvAddressable, fmt.Sprintf("%s and %s share fence %d", a.ID, other, a.Fence)}
			}
			seen[a.Fence] = a.ID
		}
	}

	// I5 / H9: an acceptance is bound to exact current, non-vacuous evidence,
	// and a new acceptance only follows the review path.
	if acc := after.Accepted; acc != nil {
		cur := after.Version
		if !hasCurrentEvidence(after, acc.Attempt) || acc.Revision != cur.IntentRevision || acc.Generation != cur.CandidateGeneration || acc.Head != after.Head {
			return Violation{InvAcceptedEvidence, fmt.Sprintf("accepted %+v without current non-vacuous evidence (version=%+v, head=%q)", *acc, cur, after.Head)}
		}
		if before.Accepted == nil {
			if before.Candidate != domain.CandVerifying {
				return Violation{InvAcceptReviewed, fmt.Sprintf("accepted while the candidate was %s, not VERIFYING", before.Candidate)}
			}
			if a, ok := before.Attempts[acc.Attempt]; !ok || a.State != domain.AttemptVerified {
				return Violation{InvAcceptReviewed, fmt.Sprintf("accepted from attempt %s in state %v, not VERIFIED", acc.Attempt, a.State)}
			}
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

	// I4: no resubmission while UNKNOWN, by RETRY (submissions unchanged) or by
	// a fresh SUBMIT of the same request (review H7).
	if cmd.Kind == domain.CmdRetryEffect {
		if f, ok := before.Effects[cmd.Effect]; ok && f.State == domain.EffectUnknown {
			if after.Effects[cmd.Effect].Submissions != f.Submissions || !errors.Is(err, domain.ErrUnknownUnresolved) {
				return Violation{InvUnknownRetry, fmt.Sprintf("retry during UNKNOWN changed submissions %d -> %d (err=%v)", f.Submissions, after.Effects[cmd.Effect].Submissions, err)}
			}
		}
	}
	if cmd.Kind == domain.CmdSubmitEffect {
		for _, e := range events {
			if e.Kind != domain.EvEffectSubmitted {
				continue
			}
			for _, f := range before.Effects {
				if f.Base == e.RequestSHA && (f.State == domain.EffectUnknown || f.State == domain.EffectDispatched) {
					return Violation{InvUnknownRetry, fmt.Sprintf("submitted %s while twin %s is %s", e.Effect, f.ID, f.State)}
				}
			}
		}
	}

	// H3: review is entered only on claims backed by this attempt's current,
	// non-vacuous receipts; the check recomputes it from the before-state.
	if cmd.Kind == domain.CmdFinishAttempt && cmd.Outcome == "ready_for_review" && err == nil &&
		after.Candidate == domain.CandVerifying && before.Candidate != domain.CandVerifying {
		if len(cmd.Claims) == 0 {
			return Violation{InvArbitration, "entered review with no claims"}
		}
		for _, c := range cmd.Claims {
			found := false
			for _, e := range before.Evidence {
				if e.Attempt == cmd.Attempt && e.Receipt == c.Receipt && !e.Vacuous && e.Revision == before.Version.IntentRevision &&
					e.Generation == before.Version.CandidateGeneration && e.Head == before.Head {
					found = true
				}
			}
			if !found {
				return Violation{InvArbitration, fmt.Sprintf("entered review on %s citing %s without a current non-vacuous receipt", c.AC, c.Receipt)}
			}
		}
	}

	// H1: the effect id is derived, canonical and occurrence-suffixed.
	if cmd.Kind == domain.CmdSubmitEffect && err == nil {
		base, berr := domain.EffectBase(cmd.EffectKind, cmd.Request)
		n := 0
		for _, f := range before.Effects {
			if f.Base == base {
				n++
			}
		}
		want := domain.EffectIDFor(base, n+1)
		for _, e := range events {
			if e.Kind == domain.EvEffectSubmitted && (berr != nil || e.Effect != want || e.RequestSHA != base || e.Occurrence != n+1) {
				return Violation{InvEffectKey, fmt.Sprintf("effect %s, want %s (occurrence %d)", e.Effect, want, e.Occurrence)}
			}
		}
	}

	// I7': an unresolved attempt blocks a new start (UNKNOWN never relaunches).
	if cmd.Kind == domain.CmdStartAttempt {
		_, unresolvedBefore, _ := liveCount(before)
		if unresolvedBefore > 0 {
			for _, e := range events {
				if e.Kind == domain.EvAttemptStarted {
					return Violation{InvUnresolvedNoStart, "attempt started while another is unresolved"}
				}
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
