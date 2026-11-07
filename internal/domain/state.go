package domain

import "fmt"

// The four orthogonal state dimensions of a mission (DESIGN.md 3.4). One enum
// cannot say "waiting on an ask while the candidate is unverified and delivery
// is not ready", so each dimension is its own column.
type (
	ExecutionState string
	CandidateState string
	DeliveryState  string
	TerminalState  string
	AttemptState   string
	EffectState    string
)

const (
	ExecIdle              ExecutionState = "IDLE"
	ExecAdmitted          ExecutionState = "ADMITTED"
	ExecRunning           ExecutionState = "RUNNING"
	ExecWaiting           ExecutionState = "WAITING"
	ExecResourceExhausted ExecutionState = "RESOURCE_EXHAUSTED"
	ExecPaused            ExecutionState = "PAUSED"
	ExecBlocked           ExecutionState = "BLOCKED"

	CandNone       CandidateState = "NONE"
	CandBuilding   CandidateState = "BUILDING"
	CandUnverified CandidateState = "UNVERIFIED"
	CandVerifying  CandidateState = "VERIFYING"
	CandAccepted   CandidateState = "ACCEPTED"
	CandRejected   CandidateState = "REJECTED"

	DelivNotReady     DeliveryState = "NOT_READY"
	DelivPRReady      DeliveryState = "PR_READY"
	DelivIntegrated   DeliveryState = "INTEGRATED"
	DelivDelivered    DeliveryState = "DELIVERED"
	DelivLiveVerified DeliveryState = "LIVE_VERIFIED"

	TermNone           TerminalState = "NONE"
	TermSucceeded      TerminalState = "SUCCEEDED"
	TermFailedDefinite TerminalState = "FAILED_DEFINITE"
	TermCancelled      TerminalState = "CANCELLED"
	TermQuarantined    TerminalState = "QUARANTINED"

	// Attempt lifecycle (DESIGN.md 3.4). SUCCEEDED_UNVERIFIED is never shown as
	// done; UNKNOWN never goes straight to a relaunch.
	AttemptPending             AttemptState = "PENDING"
	AttemptAdmitted            AttemptState = "ADMITTED"
	AttemptLeased              AttemptState = "LEASED"
	AttemptPreparing           AttemptState = "PREPARING"
	AttemptRunning             AttemptState = "RUNNING"
	AttemptSucceededUnverified AttemptState = "SUCCEEDED_UNVERIFIED"
	AttemptFailedDefinite      AttemptState = "FAILED_DEFINITE"
	AttemptUnknown             AttemptState = "UNKNOWN"
	AttemptReconciling         AttemptState = "RECONCILING"
	AttemptVerified            AttemptState = "VERIFIED"
	AttemptQuarantined         AttemptState = "QUARANTINED"

	EffectDispatched     EffectState = "DISPATCHED"
	EffectConfirmed      EffectState = "CONFIRMED"
	EffectFailedDefinite EffectState = "FAILED_DEFINITE"
	EffectUnknown        EffectState = "UNKNOWN"
	EffectNotApplied     EffectState = "NOT_APPLIED"
	EffectQuarantined    EffectState = "QUARANTINED"
)

// HoldsSlot reports whether a mission in this execution state occupies one of
// the fleet's slots (R1). Waiting and paused missions keep their slot: a slot
// is the user's "active owner", and a waiting owner is still an owner.
func (e ExecutionState) HoldsSlot() bool {
	switch e {
	case ExecAdmitted, ExecRunning, ExecWaiting, ExecPaused:
		return true
	}
	return false
}

// Live reports whether an attempt in this state may still produce authoritative
// results (it holds the mission's current fence).
func (a AttemptState) Live() bool {
	switch a {
	case AttemptAdmitted, AttemptLeased, AttemptPreparing, AttemptRunning:
		return true
	}
	return false
}

// Unresolved reports whether the attempt's outcome is not yet known. A mission
// with an unresolved attempt may not start another one (plan invariant 7).
func (a AttemptState) Unresolved() bool {
	return a == AttemptUnknown || a == AttemptReconciling || a == AttemptSucceededUnverified
}

// MaxLiveAttempts bounds attempts per mission: one writer and one reviewer (R4).
const MaxLiveAttempts = 2

// Attempt is one authorised run of a mission under one fence.
type Attempt struct {
	ID         AttemptID    `json:"id"`
	Fence      int64        `json:"fence"`
	Revision   int64        `json:"revision"`
	Generation int64        `json:"generation"`
	Host       HostID       `json:"host,omitempty"`
	Role       string       `json:"role,omitempty"`
	State      AttemptState `json:"state"`
}

// Evidence is a receipt bound to an exact subject (plan invariant 12).
// Vacuous marks a test receipt with passed = failed = 0 (R33): it is kept
// but can never back an acceptance or a ready_for_review claim.
type Evidence struct {
	Attempt    AttemptID `json:"attempt"`
	Revision   int64     `json:"revision"`
	Generation int64     `json:"generation"`
	Head       string    `json:"head"`
	Receipt    string    `json:"receipt"`
	Vacuous    bool      `json:"vacuous,omitempty"`
}

// Current reports whether the evidence is bound to the mission's exact
// current subject and carries a real result.
func (e Evidence) Current(m Mission) bool {
	return !e.Vacuous && e.Revision == m.Version.IntentRevision &&
		e.Generation == m.Version.CandidateGeneration && e.Head == m.Head
}

// EvidenceKey names the exact subject a receipt is bound to. Evidence is
// never overwritten: a later receipt for an older subject lives beside the
// current one instead of masking it (plan invariant 12).
func EvidenceKey(attempt AttemptID, revision, generation int64, head, receipt string) string {
	// Length-prefixed fields: a '|' inside head or receipt cannot make two
	// subjects share a key (review RV-17).
	return fmt.Sprintf("%d:%s|r%d|g%d|%d:%s|%d:%s", len(attempt), attempt, revision, generation, len(head), head, len(receipt), receipt)
}

// Acceptance records which attempt's candidate was accepted and for which
// exact (revision, generation, head).
// LiveTest is a live_test receipt bound to a deployed commit (AC-6).
type LiveTest struct {
	Generation int64  `json:"generation"`
	Head       string `json:"head"`
	Host       HostID `json:"host"`
	Target     string `json:"target"`
	Receipt    string `json:"receipt"`
}

type Acceptance struct {
	Attempt    AttemptID `json:"attempt"`
	Revision   int64     `json:"revision"`
	Generation int64     `json:"generation"`
	Head       string    `json:"head"`
}

// Effect is the reducer's view of an external effect: enough to refuse a retry
// while it is UNKNOWN. The broker's full state machine (approval, request
// hash, readback) is a later slice.
type Effect struct {
	ID          EffectID    `json:"id"`
	Kind        string      `json:"kind"`
	Base        string      `json:"base"`       // sha256(kind, canonical request) (H1)
	Occurrence  int         `json:"occurrence"` // 1 for the first identical request, 2 for the next
	Attempt     AttemptID   `json:"attempt"`
	Fence       int64       `json:"fence"`
	State       EffectState `json:"state"`
	Submissions int         `json:"submissions"`
}

// Mission is the reducer's state for one mission. It is the fold of the
// mission's events; the store keeps a snapshot of it in the same transaction as
// the events, and a rebuild from events must reproduce it exactly.
type Mission struct {
	ID        MissionID      `json:"id"`
	Seq       int64          `json:"seq"`
	Version   Version        `json:"version"`
	Execution ExecutionState `json:"execution"`
	Candidate CandidateState `json:"candidate"`
	Delivery  DeliveryState  `json:"delivery"`
	Terminal  TerminalState  `json:"terminal"`
	Head      string         `json:"head,omitempty"`
	WaitKey   string         `json:"wait_key,omitempty"`

	Attempts            map[AttemptID]Attempt `json:"attempts,omitempty"`
	Evidence            map[string]Evidence   `json:"evidence,omitempty"` // keyed by EvidenceKey: append-only per exact subject
	Wakes               map[string]bool       `json:"wakes,omitempty"`
	Effects             map[EffectID]Effect   `json:"effects,omitempty"`
	Accepted            *Acceptance           `json:"accepted,omitempty"`
	DeliveredGeneration *int64                `json:"delivered_generation,omitempty"`
	LiveTest            *LiveTest             `json:"live_test,omitempty"` // AC-6: the receipt delivery LIVE_VERIFIED rests on

	// Red-team F1: consecutive attempts abandoned before their runner started,
	// the backoff now in force, and why the mission was parked.
	PrelaunchFailures int    `json:"prelaunch_failures,omitempty"`
	CapacityWaits     int    `json:"capacity_waits,omitempty"` // consecutive capacity waits: backoff only, never parked (D2)
	DeferMS           int64  `json:"defer_ms,omitempty"`
	ParkedReason      string `json:"parked_reason,omitempty"`
}

// NewMission is the zero state before any event.
func NewMission(id MissionID) Mission {
	return Mission{
		ID:        id,
		Execution: ExecIdle,
		Candidate: CandNone,
		Delivery:  DelivNotReady,
		Terminal:  TermNone,
	}
}

// Clone returns a deep copy so reducers never alias their input.
func (m Mission) Clone() Mission {
	c := m
	c.Attempts = cloneMap(m.Attempts)
	c.Evidence = cloneMap(m.Evidence)
	c.Wakes = cloneMap(m.Wakes)
	c.Effects = cloneMap(m.Effects)
	if m.Accepted != nil {
		a := *m.Accepted
		c.Accepted = &a
	}
	if m.DeliveredGeneration != nil {
		g := *m.DeliveredGeneration
		c.DeliveredGeneration = &g
	}
	return c
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	if in == nil {
		return nil
	}
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// LiveAttempts counts attempts that still hold authority.
func (m Mission) LiveAttempts() int {
	n := 0
	for _, a := range m.Attempts {
		if a.State.Live() {
			n++
		}
	}
	return n
}

// CurrentEvidence returns the receipts bound to the current subject for one
// attempt, sorted by receipt id for determinism.
func (m Mission) CurrentEvidence(attempt AttemptID) []Evidence {
	var out []Evidence
	for _, e := range m.Evidence {
		if e.Attempt == attempt && e.Current(m) {
			out = append(out, e)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Receipt < out[j-1].Receipt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Occurrences counts effects already recorded with the given base.
func (m Mission) Occurrences(base string) int {
	n := 0
	for _, f := range m.Effects {
		if f.Base == base {
			n++
		}
	}
	return n
}

// UnresolvedAttempts counts attempts whose outcome is not yet known.
func (m Mission) UnresolvedAttempts() int {
	n := 0
	for _, a := range m.Attempts {
		if a.State.Unresolved() {
			n++
		}
	}
	return n
}

// Pre-launch backoff (red-team F1). Deterministic: a pure function of the
// failure count, so replay reproduces it.
const (
	PrelaunchBudget = 6 // consecutive pre-launch failures before the mission is parked
	PrelaunchBaseMS = 2000
	PrelaunchMaxMS  = 5 * 60 * 1000
)

// PrelaunchBackoffMS is the wait after the n-th consecutive pre-launch failure.
func PrelaunchBackoffMS(n int) int64 {
	b := int64(PrelaunchBaseMS)
	for i := 1; i < n && b < PrelaunchMaxMS; i++ {
		b *= 2
	}
	if b > PrelaunchMaxMS {
		b = PrelaunchMaxMS
	}
	return b
}
