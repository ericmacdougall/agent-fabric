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
		e := ev(domain.EvEvidenceRecorded)
		e.Attempt = a.ID
		e.Fence = a.Fence
		e.Revision = cmd.Revision
		e.Generation = cmd.Generation
		e.Head = cmd.Head
		e.Receipt = cmd.Receipt
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
		case "checkpoint", "ready_for_review", "blocked":
			e.AttemptTo = domain.AttemptSucceededUnverified
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
		default:
			return nil, fmt.Errorf("%w: unknown outcome %q", domain.ErrInvalidTransition, cmd.Outcome)
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
		if m.Candidate != domain.CandUnverified && m.Candidate != domain.CandVerifying {
			return nil, fmt.Errorf("%w: accept while candidate %s", domain.ErrInvalidTransition, m.Candidate)
		}
		a, ok := m.Attempts[cmd.Attempt]
		if !ok {
			return nil, fmt.Errorf("%w: attempt %s", domain.ErrNotFound, cmd.Attempt)
		}
		if a.State.Unresolved() || a.State == domain.AttemptFailedDefinite || a.State == domain.AttemptQuarantined {
			return nil, fmt.Errorf("%w: accept from attempt in %s", domain.ErrInvalidTransition, a.State)
		}
		evd, ok := m.Evidence[domain.EvidenceKey(a.ID, m.Version.IntentRevision, m.Version.CandidateGeneration, m.Head)]
		if !ok {
			return nil, domain.ErrEvidenceStale
		}
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
		if cmd.Effect == "" {
			return nil, fmt.Errorf("%w: effect id", domain.ErrInvalidTransition)
		}
		if _, dup := m.Effects[cmd.Effect]; dup {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrDuplicate, cmd.Effect)
		}
		e := ev(domain.EvEffectSubmitted)
		e.Effect = cmd.Effect
		e.Attempt = a.ID
		e.Fence = a.Fence
		return []domain.Event{e}, nil

	case domain.CmdEffectUnknown:
		f, ok := m.Effects[cmd.Effect]
		if !ok {
			return nil, fmt.Errorf("%w: effect %s", domain.ErrNotFound, cmd.Effect)
		}
		if f.State != domain.EffectDispatched {
			return nil, fmt.Errorf("%w: effect %s is %s", domain.ErrInvalidTransition, f.ID, f.State)
		}
		e := ev(domain.EvEffectUnknown)
		e.Effect = f.ID
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
		if f.State != domain.EffectDispatched && f.State != domain.EffectUnknown {
			return nil, fmt.Errorf("%w: resolve while %s", domain.ErrInvalidTransition, f.State)
		}
		switch domain.EffectState(cmd.Result) {
		case domain.EffectConfirmed, domain.EffectFailedDefinite, domain.EffectNotApplied, domain.EffectQuarantined:
		default:
			return nil, fmt.Errorf("%w: effect result %q", domain.ErrInvalidTransition, cmd.Result)
		}
		e := ev(domain.EvEffectResolved)
		e.Effect = f.ID
		e.Result = cmd.Result
		return []domain.Event{e}, nil

	case domain.CmdRelease:
		if !m.Execution.HoldsSlot() {
			return nil, fmt.Errorf("%w: release while %s", domain.ErrInvalidTransition, m.Execution)
		}
		if m.LiveAttempts() > 0 {
			return nil, fmt.Errorf("%w: live attempts", domain.ErrAttemptLive)
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

// fenceCheck refuses any message whose fence is not the attempt's own fence.
// A smaller fence is stale (a late message from a superseded run); a larger one
// is forged or from the future and is refused the same way.
func fenceCheck(m domain.Mission, a domain.Attempt, fence int64) error {
	if fence != a.Fence || m.Version.FenceIsStale(fence) {
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
		switch e.Outcome {
		case "wait":
			if s.Execution.HoldsSlot() {
				s.Execution = domain.ExecWaiting
				s.WaitKey = e.WaitKey
			}
		case "blocked":
			s.Execution = domain.ExecBlocked // frees the slot; re-entry is an admission
			s.WaitKey = ""
		default:
			if s.Execution == domain.ExecRunning && s.LiveAttempts() == 0 {
				s.Execution = domain.ExecAdmitted
			}
		}
	case domain.EvEvidenceRecorded:
		if s.Evidence == nil {
			s.Evidence = map[string]domain.Evidence{}
		}
		s.Evidence[domain.EvidenceKey(e.Attempt, e.Revision, e.Generation, e.Head)] = domain.Evidence{
			Attempt: e.Attempt, Revision: e.Revision, Generation: e.Generation, Head: e.Head, Receipt: e.Receipt,
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
		s.Effects[e.Effect] = domain.Effect{ID: e.Effect, Attempt: e.Attempt, Fence: e.Fence, State: domain.EffectDispatched, Submissions: 1}
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

// Replay folds a mission's event log from the zero state.
func Replay(id domain.MissionID, events []domain.Event) domain.Mission {
	m := domain.NewMission(id)
	for _, e := range events {
		m = Apply(m, e)
	}
	return m
}
