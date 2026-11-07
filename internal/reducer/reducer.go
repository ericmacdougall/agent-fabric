// Package reducer is the fabric's pure decision core (DESIGN.md 2.5, 5.3).
//
// Decide turns a command into zero or more events or a refusal; Apply folds
// one event into the mission state. Neither touches a clock, a database or a
// process, so every safety invariant is testable as a property of pure code
// and every mutant is a one-line change in this file.
//
// The store runs Decide and Apply inside one transaction, assigns the
// per-mission sequence and writes the events, the outbox rows and the state
// snapshot together.
package reducer

import (
	"fmt"

	"fabric/internal/domain"
)

// Env is what the reducer needs from outside the mission: the fleet cap and
// how many other missions currently hold a slot. The store computes both under
// the slots row lock in the same transaction (AC-1: the cap is a transaction).
type Env struct {
	Cap       int
	SlotsUsed int // missions other than this one that hold a slot
}

// Decide returns the events a command produces, or a refusal. A refusal is a
// recorded response: the caller stores it under the command's key.
// Decide never mutates m.
func Decide(m domain.Mission, cmd domain.Command, env Env) ([]domain.Event, error) {
	if cmd.Mission != m.ID {
		return nil, fmt.Errorf("%w: command for %q applied to %q", domain.ErrInvalidTransition, cmd.Mission, m.ID)
	}
	ev := func(kind domain.EventKind) domain.Event {
		return domain.Event{Kind: kind, Mission: m.ID}
	}
	if m.Terminal != domain.TermNone {
		return nil, fmt.Errorf("%w: mission is terminal (%s)", domain.ErrInvalidTransition, m.Terminal)
	}

	switch cmd.Kind {
	case domain.CmdAdmitMission:
		if m.Execution.HoldsSlot() {
			return nil, fmt.Errorf("%w: already %s", domain.ErrInvalidTransition, m.Execution)
		}
		if env.SlotsUsed >= env.Cap {
			return nil, fmt.Errorf("%w: %d of %d slots held", domain.ErrOverCapacity, env.SlotsUsed, env.Cap)
		}
		return []domain.Event{ev(domain.EvMissionAdmitted)}, nil

	case domain.CmdReviseIntent:
		e := ev(domain.EvIntentRevised)
		e.Revision = m.Version.IntentRevision + 1
		e.Reason = cmd.Reason
		out := []domain.Event{e}
		if m.Accepted != nil {
			r := ev(domain.EvAcceptanceRevoked)
			r.Attempt = m.Accepted.Attempt
			r.Reason = "intent revised"
			out = append(out, r)
		}
		return out, nil

	case domain.CmdStartAttempt:
		if m.Execution != domain.ExecAdmitted && m.Execution != domain.ExecRunning {
			return nil, fmt.Errorf("%w: cannot start an attempt while %s", domain.ErrInvalidTransition, m.Execution)
		}
		if _, dup := m.Attempts[cmd.Attempt]; dup {
			return nil, fmt.Errorf("%w: attempt %s", domain.ErrDuplicate, cmd.Attempt)
		}
		if m.UnresolvedAttempts() > 0 {
			return nil, fmt.Errorf("%w: an attempt is unresolved", domain.ErrUnknownUnresolved)
		}
		if m.LiveAttempts() >= domain.MaxLiveAttempts {
			return nil, fmt.Errorf("%w: %d live attempts", domain.ErrAttemptLive, m.LiveAttempts())
		}
		for _, other := range m.Attempts {
			if other.State.Live() && other.Role == cmd.Role {
				// One live attempt per role (writer, reviewer): a second START
				// never supersedes a live run silently (review H6).
				return nil, fmt.Errorf("%w: %s %s is live", domain.ErrAttemptLive, cmd.Role, other.ID)
			}
		}
		e := ev(domain.EvAttemptStarted)
		e.Attempt = cmd.Attempt
		e.Fence = m.Version.AttemptFence + 1
		e.Revision = m.Version.IntentRevision
		e.Generation = m.Version.CandidateGeneration
		e.Host = cmd.Host
		e.Role = cmd.Role
		e.AttemptTo = domain.AttemptLeased
		return []domain.Event{e}, nil

	case domain.CmdAckStart:
		a, err := liveAttempt(m, cmd)
		if err != nil {
			return nil, err
		}
		if a.State != domain.AttemptLeased && a.State != domain.AttemptPreparing {
			return nil, fmt.Errorf("%w: ack while %s", domain.ErrInvalidTransition, a.State)
		}
		e := ev(domain.EvAttemptAcked)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.AttemptTo = domain.AttemptRunning
		return []domain.Event{e}, nil

	case domain.CmdRecordEvidence:
		a, err := liveAttempt(m, cmd)
		if err != nil {
			return nil, err
		}
		if cmd.Receipt == "" {
			return nil, fmt.Errorf("%w: evidence needs a receipt", domain.ErrInvalidTransition)
		}
		if cmd.Revision > m.Version.IntentRevision || cmd.Generation > m.Version.CandidateGeneration {
			return nil, fmt.Errorf("%w: evidence for a subject that does not exist yet", domain.ErrEvidenceStale)
		}
		e := ev(domain.EvEvidenceRecorded)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.Revision = cmd.Revision
		e.Generation = cmd.Generation
		e.Head = cmd.Head
		e.Receipt = cmd.Receipt
		e.Vacuous = cmd.Vacuous
		return []domain.Event{e}, nil

	case domain.CmdFinishAttempt:
		a, err := liveAttempt(m, cmd)
		if err != nil {
			return nil, err
		}
		e := ev(domain.EvAttemptFinished)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.Outcome = cmd.Outcome
		e.WaitKey = cmd.WaitKey
		e.Reason = cmd.Reason
		switch cmd.Outcome {
		case "checkpoint", "blocked":
			e.AttemptTo = domain.AttemptSucceededUnverified
		case "ready_for_review":
			e.AttemptTo = domain.AttemptSucceededUnverified
			e.Claims = cmd.Claims
			if why := arbitrate(m, a, cmd.Claims); why != "" {
				// H3: a ready_for_review claim is a claim, not truth. The
				// checkpoint is kept, the refusal is recorded as an alert
				// (R28) and the mission does not enter review.
				e.Outcome = "checkpoint"
				e.Reason = "ready_for_review refused: " + why
				r := ev(domain.EvClaimRefused)
				r.Attempt = a.ID
				r.Fence = a.Fence
				r.Reason = why
				r.Claims = cmd.Claims
				return []domain.Event{e, r}, nil
			}
		case "wait":
			if cmd.WaitKey == "" {
				return nil, fmt.Errorf("%w: wait needs a wait key", domain.ErrInvalidTransition)
			}
			e.AttemptTo = domain.AttemptSucceededUnverified
			if m.Wakes[cmd.WaitKey] {
				// The event already arrived: wait and resume in one step, so a
				// wake that raced the finish is never lost (E6: notifies are
				// hints; the table is the record).
				r := ev(domain.EvMissionResumed)
				r.Token = cmd.WaitKey
				return []domain.Event{e, r}, nil
			}
		case "abandoned":
			e.AttemptTo = domain.AttemptFailedDefinite
			if cmd.Prelaunch {
				// Red-team F1: nothing ran, so nothing changed that would make
				// the next attempt succeed. Back off; after a run of them, park.
				e.Prelaunch = true
				if cmd.CapacityWait {
					// Red-team D2: a full resource is backpressure. Back off
					// (capped) on its own streak; it never counts toward parking.
					d := ev(domain.EvDispatchDeferred)
					d.Wait = true
					d.Count = m.CapacityWaits + 1
					d.BackoffMS = domain.PrelaunchBackoffMS(d.Count)
					return []domain.Event{e, d}, nil
				}
				n := m.PrelaunchFailures + 1
				if n >= domain.PrelaunchBudget {
					p := ev(domain.EvMissionParked)
					p.Count = n
					p.Reason = fmt.Sprintf("%d consecutive attempts abandoned before launch; last: %s", n, cmd.Reason)
					return []domain.Event{e, p}, nil
				}
				d := ev(domain.EvDispatchDeferred)
				d.Count = n
				d.BackoffMS = domain.PrelaunchBackoffMS(n)
				return []domain.Event{e, d}, nil
			}
		default:
			return nil, fmt.Errorf("%w: unknown outcome %q", domain.ErrInvalidTransition, cmd.Outcome)
		}
		if cmd.Prelaunch || cmd.CapacityWait {
			return nil, fmt.Errorf("%w: prelaunch is only an abandonment", domain.ErrInvalidTransition)
		}
		return []domain.Event{e}, nil

	case domain.CmdExpireLease:
		a, err := liveAttempt(m, cmd)
		if err != nil {
			return nil, err
		}
		e := ev(domain.EvAttemptUnknown)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.Reason = "lease expired"
		e.AttemptTo = domain.AttemptUnknown
		return []domain.Event{e}, nil

	case domain.CmdReconcileAttempt:
		a, ok := m.Attempts[cmd.Attempt]
		if !ok {
			return nil, fmt.Errorf("%w: attempt %s", domain.ErrNotFound, cmd.Attempt)
		}
		if err := fenceCheck(m, a, cmd.Fence); err != nil {
			return nil, err
		}
		if !a.State.Unresolved() {
			return nil, fmt.Errorf("%w: reconcile while %s", domain.ErrInvalidTransition, a.State)
		}
		var to domain.AttemptState
		switch cmd.Result {
		case "VERIFIED":
			to = domain.AttemptVerified
		case "FAILED_DEFINITE":
			to = domain.AttemptFailedDefinite
		case "QUARANTINED":
			to = domain.AttemptQuarantined
		default:
			return nil, fmt.Errorf("%w: reconcile result %q", domain.ErrInvalidTransition, cmd.Result)
		}
		e := ev(domain.EvAttemptReconciled)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.Result = cmd.Result
		e.AttemptTo = to
		return []domain.Event{e}, nil

	case domain.CmdSelectCandidate:
		if cmd.Head == "" {
			return nil, fmt.Errorf("%w: candidate needs a head", domain.ErrInvalidTransition)
		}
		e := ev(domain.EvCandidateSelected)
		e.Generation = m.Version.CandidateGeneration + 1
		e.Head = cmd.Head
		e.Attempt = cmd.Attempt
		return []domain.Event{e}, nil

	case domain.CmdAccept:
		// Acceptance is the end of the review path: the candidate must have
		// entered VERIFYING through an arbitrated ready_for_review claim and
		// the attempt must be finished and VERIFIED (review H9; DESIGN 3.4
		// "only VERIFIED runs advance the mission").
		if m.Candidate != domain.CandVerifying {
			return nil, fmt.Errorf("%w: accept while candidate %s (needs VERIFYING)", domain.ErrInvalidTransition, m.Candidate)
		}
		a, ok := m.Attempts[cmd.Attempt]
		if !ok {
			return nil, fmt.Errorf("%w: attempt %s", domain.ErrNotFound, cmd.Attempt)
		}
		if a.State != domain.AttemptVerified {
			return nil, fmt.Errorf("%w: accept from attempt in %s (needs VERIFIED)", domain.ErrInvalidTransition, a.State)
		}
		cur := m.CurrentEvidence(a.ID)
		if len(cur) == 0 {
			return nil, domain.ErrEvidenceStale
		}
		evd := cur[0]
		e := ev(domain.EvCandidateAccepted)
		e.Attempt = a.ID
		e.Revision = m.Version.IntentRevision
		e.Generation = m.Version.CandidateGeneration
		e.Head = m.Head
		e.Receipt = evd.Receipt
		return []domain.Event{e}, nil

	case domain.CmdDeliver:
		acc := m.Accepted
		if acc == nil || acc.Attempt != cmd.Attempt {
			return nil, fmt.Errorf("%w: no accepted candidate from %s", domain.ErrInvalidTransition, cmd.Attempt)
		}
		if acc.Revision != m.Version.IntentRevision || acc.Generation != m.Version.CandidateGeneration {
			return nil, domain.ErrEvidenceStale
		}
		if cmd.Generation != m.Version.CandidateGeneration {
			return nil, domain.ErrGenerationStale
		}
		e := ev(domain.EvDelivered)
		e.Attempt = acc.Attempt
		e.Generation = cmd.Generation
		e.Head = acc.Head
		return []domain.Event{e}, nil

	case domain.CmdWake:
		if cmd.Token == "" {
			return nil, fmt.Errorf("%w: wake needs a token", domain.ErrInvalidTransition)
		}
		if m.Wakes[cmd.Token] {
			return nil, nil // duplicate wake: idempotent, no new fact
		}
		e := ev(domain.EvWakeRecorded)
		e.Token = cmd.Token
		out := []domain.Event{e}
		if m.Execution == domain.ExecWaiting && m.WaitKey == cmd.Token {
			r := ev(domain.EvMissionResumed)
			r.Token = cmd.Token
			out = append(out, r)
		}
		return out, nil

	case domain.CmdAckWake:
		if !m.Wakes[cmd.Token] {
			return nil, fmt.Errorf("%w: wake %q", domain.ErrNotFound, cmd.Token)
		}
		e := ev(domain.EvWakeAcked)
		e.Token = cmd.Token
		return []domain.Event{e}, nil

	case domain.CmdSubmitEffect:
		a, err := liveAttempt(m, cmd)
		if err != nil {
			return nil, err
		}
		// H1: the id is a pure function of (kind, canonical request) plus the
		// occurrence, so two identical requests in one mission are two
		// effects, while a replayed command (same key) is served by the store
		// and never reaches here twice.
		base, err := domain.EffectBase(cmd.EffectKind, cmd.Request)
		if err != nil {
			return nil, err
		}
		for _, f := range m.Effects {
			if f.Base != base {
				continue
			}
			switch f.State {
			case domain.EffectUnknown:
				return nil, fmt.Errorf("%w: %s is UNKNOWN; reconcile before submitting the same request again", domain.ErrUnknownUnresolved, f.ID)
			case domain.EffectDispatched:
				return nil, fmt.Errorf("%w: %s is DISPATCHED", domain.ErrDuplicate, f.ID)
			}
		}
		occurrence := m.Occurrences(base) + 1
		id := domain.EffectIDFor(base, occurrence)
		if _, dup := m.Effects[id]; dup {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrDuplicate, id)
		}
		e := ev(domain.EvEffectSubmitted)
		e.Effect = id
		e.EffectKind = cmd.EffectKind
		e.RequestSHA = base
		e.Occurrence = occurrence
		e.Attempt = a.ID
		e.Fence = a.Fence
		return []domain.Event{e}, nil

	case domain.CmdEffectUnknown:
		f, ok := m.Effects[cmd.Effect]
		if !ok {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrNotFound, cmd.Effect)
		}
		if err := sameSubmission(f, cmd); err != nil {
			return nil, err
		}
		if f.State != domain.EffectDispatched {
			return nil, fmt.Errorf("%w: effect %s is %s", domain.ErrInvalidTransition, f.ID, f.State)
		}
		e := ev(domain.EvEffectUnknown)
		e.Effect = f.ID
		e.Submission = cmd.Submission
		e.Reason = cmd.Reason
		return []domain.Event{e}, nil

	case domain.CmdRetryEffect:
		f, ok := m.Effects[cmd.Effect]
		if !ok {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrNotFound, cmd.Effect)
		}
		if f.State == domain.EffectUnknown {
			return nil, domain.ErrUnknownUnresolved
		}
		if f.State != domain.EffectFailedDefinite && f.State != domain.EffectNotApplied {
			return nil, fmt.Errorf("%w: retry while %s", domain.ErrInvalidTransition, f.State)
		}
		e := ev(domain.EvEffectResubmitted)
		e.Effect = f.ID
		return []domain.Event{e}, nil

	case domain.CmdResolveEffect:
		f, ok := m.Effects[cmd.Effect]
		if !ok {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrNotFound, cmd.Effect)
		}
		if err := sameSubmission(f, cmd); err != nil {
			return nil, err
		}
		// Transport may confirm, fail definitely or conflict; only a readback
		// of an UNKNOWN effect may say NOT_APPLIED, and an UNKNOWN effect is
		// never FAILED_DEFINITE, which would re-arm a retry without proof
		// that the first submission did not land (Kimi effect-identity).
		allowed := map[domain.EffectState][]domain.EffectState{
			domain.EffectDispatched: {domain.EffectConfirmed, domain.EffectFailedDefinite, domain.EffectQuarantined},
			domain.EffectUnknown:    {domain.EffectConfirmed, domain.EffectNotApplied, domain.EffectQuarantined},
		}[f.State]
		if allowed == nil {
			return nil, fmt.Errorf("%w: resolve while %s", domain.ErrInvalidTransition, f.State)
		}
		ok = false
		for _, r := range allowed {
			ok = ok || r == domain.EffectState(cmd.Result)
		}
		if !ok {
			return nil, fmt.Errorf("%w: effect %s %s -> %q", domain.ErrInvalidTransition, f.ID, f.State, cmd.Result)
		}
		e := ev(domain.EvEffectResolved)
		e.Effect = f.ID
		e.Submission = cmd.Submission
		e.Result = cmd.Result
		return []domain.Event{e}, nil

	case domain.CmdRelease:
		if !m.Execution.HoldsSlot() {
			return nil, fmt.Errorf("%w: release while %s", domain.ErrInvalidTransition, m.Execution)
		}
		if m.LiveAttempts() > 0 {
			return nil, fmt.Errorf("%w: live attempts", domain.ErrAttemptLive)
		}
		if m.UnresolvedAttempts() > 0 {
			return nil, fmt.Errorf("%w: an attempt is unresolved; the slot is held until it is reconciled", domain.ErrUnknownUnresolved)
		}
		return []domain.Event{ev(domain.EvMissionReleased)}, nil
	}
	return nil, fmt.Errorf("%w: unknown command %q", domain.ErrInvalidTransition, cmd.Kind)
}

// liveAttempt resolves the attempt a command names, checks its fence and that
// it still holds authority.
func liveAttempt(m domain.Mission, cmd domain.Command) (domain.Attempt, error) {
	a, ok := m.Attempts[cmd.Attempt]
	if !ok {
		return a, fmt.Errorf("%w: attempt %s", domain.ErrNotFound, cmd.Attempt)
	}
	if err := fenceCheck(m, a, cmd.Fence); err != nil {
		return a, err
	}
	if !a.State.Live() {
		return a, fmt.Errorf("%w: attempt %s is %s", domain.ErrInvalidTransition, a.ID, a.State)
	}
	return a, nil
}

// arbitrate (H3, REPORT.md:320-327) checks a ready_for_review claim against
// the mission's own evidence: every claimed AC must cite a receipt recorded
// by this attempt for the exact current (revision, generation, head), and the
// receipt must not be vacuous (R33). It returns the reason the claim fails,
// or "" when it holds.
func arbitrate(m domain.Mission, a domain.Attempt, claims []domain.Claim) string {
	if m.Candidate == domain.CandNone || m.Head == "" {
		return "no candidate head selected"
	}
	if len(claims) == 0 {
		return "no acceptance claims"
	}
	for _, c := range claims {
		if c.AC == "" || c.Receipt == "" {
			return "claim without an AC or a receipt"
		}
		evd, ok := m.Evidence[domain.EvidenceKey(a.ID, m.Version.IntentRevision, m.Version.CandidateGeneration, m.Head, c.Receipt)]
		if !ok {
			return fmt.Sprintf("%s cites %s, which is not this attempt's evidence for the current head", c.AC, c.Receipt)
		}
		if evd.Vacuous {
			return fmt.Sprintf("%s cites %s, a vacuous receipt (passed = failed = 0)", c.AC, c.Receipt)
		}
	}
	return ""
}

// fenceCheck refuses any message whose fence is not the attempt's own fence.
// A smaller fence is stale (a late message from a superseded run); a larger one
// is forged or from the future and is refused the same way.
// A fence names one attempt: every live attempt keeps its own fence until it
// ends, so a writer and a reviewer running side by side both stay
// addressable (review H6); superseding a run is a new attempt id, never a
// silent fence bump.
func fenceCheck(m domain.Mission, a domain.Attempt, fence int64) error {
	if fence != a.Fence {
		return fmt.Errorf("%w: got %d, attempt %s holds %d, mission at %d",
			domain.ErrStaleFence, fence, a.ID, a.Fence, m.Version.AttemptFence)
	}
	return nil
}

// Apply folds one event into the state and returns the new state. It is total
// over events Decide can emit; an unknown kind is returned unchanged so a
// replay of a newer log by an older binary cannot invent state.
func Apply(m domain.Mission, e domain.Event) domain.Mission {
	s := m.Clone()
	if e.Seq > 0 {
		s.Seq = e.Seq
	}
	switch e.Kind {
	case domain.EvMissionAdmitted:
		s.Execution = domain.ExecAdmitted
		s.PrelaunchFailures, s.CapacityWaits, s.DeferMS, s.ParkedReason = 0, 0, 0, "" // re-admission is the operator's reset
	case domain.EvDispatchDeferred:
		if e.Wait {
			s.CapacityWaits, s.DeferMS = e.Count, e.BackoffMS
		} else {
			s.PrelaunchFailures, s.DeferMS = e.Count, e.BackoffMS
		}
	case domain.EvMissionParked:
		s.PrelaunchFailures, s.DeferMS, s.ParkedReason = e.Count, 0, e.Reason
		if s.LiveAttempts() == 0 && s.UnresolvedAttempts() == 0 {
			s.Execution = domain.ExecBlocked // frees the slot; re-entry is an admission
		}
	case domain.EvIntentRevised:
		s.Version.IntentRevision = e.Revision
	case domain.EvAcceptanceRevoked:
		s.Accepted = nil
		if s.Candidate == domain.CandAccepted {
			s.Candidate = domain.CandUnverified
		}
	case domain.EvAttemptStarted:
		if s.Attempts == nil {
			s.Attempts = map[domain.AttemptID]domain.Attempt{}
		}
		s.Version.AttemptFence = e.Fence
		s.Attempts[e.Attempt] = domain.Attempt{
			ID: e.Attempt, Fence: e.Fence, Revision: e.Revision, Generation: e.Generation,
			Host: e.Host, Role: e.Role, State: domain.AttemptLeased,
		}
		s.Execution = domain.ExecRunning
	case domain.EvAttemptAcked, domain.EvAttemptUnknown, domain.EvAttemptReconciled:
		a := s.Attempts[e.Attempt]
		a.State = e.AttemptTo
		s.Attempts[e.Attempt] = a
		// Only MISSION_ADMITTED may move a mission into a slot-holding state
		// (the cap is checked there and nowhere else), so a quarantine pauses
		// a mission that holds a slot and leaves a BLOCKED one where it is.
		if e.Kind == domain.EvAttemptReconciled && e.AttemptTo == domain.AttemptQuarantined && s.Execution.HoldsSlot() {
			s.Execution = domain.ExecPaused
		}
	case domain.EvAttemptFinished:
		a := s.Attempts[e.Attempt]
		a.State = e.AttemptTo
		s.Attempts[e.Attempt] = a
		if !e.Prelaunch {
			s.PrelaunchFailures, s.CapacityWaits, s.DeferMS = 0, 0, 0 // a runner ran: the run of failures is over
		}
		switch e.Outcome {
		case "wait":
			if s.Execution.HoldsSlot() {
				s.Execution = domain.ExecWaiting
				s.WaitKey = e.WaitKey
			}
		case "blocked":
			if s.LiveAttempts() == 0 && s.UnresolvedAttempts() == 0 {
				s.Execution = domain.ExecBlocked // frees the slot; re-entry is an admission
				s.WaitKey = ""
			}
		default:
			if e.Outcome == "ready_for_review" && s.Candidate == domain.CandUnverified {
				s.Candidate = domain.CandVerifying
			}
			if s.Execution == domain.ExecRunning && s.LiveAttempts() == 0 {
				s.Execution = domain.ExecAdmitted
			}
		}
	case domain.EvEvidenceRecorded:
		if s.Evidence == nil {
			s.Evidence = map[string]domain.Evidence{}
		}
		s.Evidence[domain.EvidenceKey(e.Attempt, e.Revision, e.Generation, e.Head, e.Receipt)] = domain.Evidence{
			Attempt: e.Attempt, Revision: e.Revision, Generation: e.Generation, Head: e.Head, Receipt: e.Receipt, Vacuous: e.Vacuous,
		}
	case domain.EvCandidateSelected:
		s.Version.CandidateGeneration = e.Generation
		s.Head = e.Head
		s.Candidate = domain.CandUnverified
		s.Accepted = nil
		s.DeliveredGeneration = nil
		if s.Delivery == domain.DelivDelivered || s.Delivery == domain.DelivLiveVerified {
			s.Delivery = domain.DelivNotReady
		}
	case domain.EvCandidateAccepted:
		s.Candidate = domain.CandAccepted
		s.Accepted = &domain.Acceptance{Attempt: e.Attempt, Revision: e.Revision, Generation: e.Generation, Head: e.Head}
	case domain.EvDelivered:
		g := e.Generation
		s.DeliveredGeneration = &g
		s.Delivery = domain.DelivDelivered
	case domain.EvWakeRecorded:
		if s.Wakes == nil {
			s.Wakes = map[string]bool{}
		}
		s.Wakes[e.Token] = true
	case domain.EvWakeAcked:
		delete(s.Wakes, e.Token)
	case domain.EvMissionResumed:
		if s.Execution == domain.ExecWaiting {
			s.Execution = domain.ExecAdmitted
			s.WaitKey = ""
		}
	case domain.EvEffectSubmitted:
		if s.Effects == nil {
			s.Effects = map[domain.EffectID]domain.Effect{}
		}
		s.Effects[e.Effect] = domain.Effect{ID: e.Effect, Kind: e.EffectKind, Base: e.RequestSHA, Occurrence: e.Occurrence,
			Attempt: e.Attempt, Fence: e.Fence, State: domain.EffectDispatched, Submissions: 1}
	case domain.EvEffectUnknown:
		f := s.Effects[e.Effect]
		f.State = domain.EffectUnknown
		s.Effects[e.Effect] = f
	case domain.EvEffectResubmitted:
		f := s.Effects[e.Effect]
		f.State = domain.EffectDispatched
		f.Submissions++
		s.Effects[e.Effect] = f
	case domain.EvEffectResolved:
		f := s.Effects[e.Effect]
		f.State = domain.EffectState(e.Result)
		s.Effects[e.Effect] = f
	case domain.EvMissionReleased:
		s.Execution = domain.ExecIdle
		s.WaitKey = ""
	}
	return s
}

// sameSubmission binds a transport or readback result to the submission it
// is about: a late result for an earlier submission is a stale fence.
func sameSubmission(f domain.Effect, cmd domain.Command) error {
	if cmd.Submission != f.Submissions {
		return fmt.Errorf("%w: result for submission %d of %s, which is on submission %d", domain.ErrStaleFence, cmd.Submission, f.ID, f.Submissions)
	}
	return nil
}

// Replay folds a mission's event log from the zero state. It is the
// authority's compatibility boundary (Kimi compatibility analysis): it stops
// at the first event kind this build cannot interpret and returns
// ErrUnsupportedEvent with the known prefix, for diagnostics only. Apply
// stays permissive for the Decide -> Apply path, which only sees kinds this
// build emits.
func Replay(id domain.MissionID, events []domain.Event) (domain.Mission, error) {
	m := domain.NewMission(id)
	for i, e := range events {
		if !domain.KnownEventKind(e.Kind) {
			return m, fmt.Errorf("%w: mission %s event %d (seq %d) is %q", domain.ErrUnsupportedEvent, id, i, e.Seq, e.Kind)
		}
		m = Apply(m, e)
	}
	return m, nil
}
