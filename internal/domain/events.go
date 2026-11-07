package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// CommandKind names what a caller asks the reducer to do.
type CommandKind string

const (
	CmdAdmitMission     CommandKind = "ADMIT_MISSION"
	CmdReviseIntent     CommandKind = "REVISE_INTENT"
	CmdStartAttempt     CommandKind = "START_ATTEMPT"
	CmdAckStart         CommandKind = "ACK_START"
	CmdRecordEvidence   CommandKind = "RECORD_EVIDENCE"
	CmdFinishAttempt    CommandKind = "FINISH_ATTEMPT"
	CmdExpireLease      CommandKind = "EXPIRE_LEASE"
	CmdReconcileAttempt CommandKind = "RECONCILE_ATTEMPT"
	CmdSelectCandidate  CommandKind = "SELECT_CANDIDATE"
	CmdAccept           CommandKind = "ACCEPT"
	CmdDeliver          CommandKind = "DELIVER"
	CmdWake             CommandKind = "WAKE"
	CmdAckWake          CommandKind = "ACK_WAKE"
	CmdSubmitEffect     CommandKind = "SUBMIT_EFFECT"
	CmdEffectUnknown    CommandKind = "EFFECT_UNKNOWN"
	CmdRetryEffect      CommandKind = "RETRY_EFFECT"
	CmdResolveEffect    CommandKind = "RESOLVE_EFFECT"
	CmdRelease          CommandKind = "RELEASE"
)

// Command is one request to change a mission. The fields used depend on Kind;
// one flat struct keeps the canonical JSON (and so the request hash) trivial.
type Command struct {
	Kind    CommandKind `json:"kind"`
	Mission MissionID   `json:"mission"`

	Attempt    AttemptID `json:"attempt,omitempty"`
	Fence      int64     `json:"fence,omitempty"`
	Revision   int64     `json:"revision,omitempty"`
	Generation int64     `json:"generation,omitempty"`
	Head       string    `json:"head,omitempty"`
	Host       HostID    `json:"host,omitempty"`
	Role       string    `json:"role,omitempty"`
	Receipt    string    `json:"receipt,omitempty"`
	Outcome    string    `json:"outcome,omitempty"` // FINISH_ATTEMPT: checkpoint|wait|ready_for_review|blocked|abandoned
	// FINISH_ATTEMPT abandoned: the runner was never started (red-team F1).
	Prelaunch bool `json:"prelaunch,omitempty"`
	// With Prelaunch: the cause is a full resource, a wait not a failure (red-team D2).
	CapacityWait bool     `json:"capacity_wait,omitempty"`
	WaitKey      string   `json:"wait_key,omitempty"`
	Token        string   `json:"token,omitempty"`
	Effect       EffectID `json:"effect,omitempty"`
	Result       string   `json:"result,omitempty"` // RECONCILE_ATTEMPT / RESOLVE_EFFECT
	// EFFECT_UNKNOWN / RESOLVE_EFFECT: the submission the result is about.
	Submission int    `json:"submission,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// SUBMIT_EFFECT (H1): the effect id is derived from kind + canonical
	// request + occurrence; the caller never names it.
	EffectKind string          `json:"effect_kind,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	// RECORD_EVIDENCE (R33): a test receipt with passed = failed = 0.
	Vacuous bool `json:"vacuous,omitempty"`
	// FINISH_ATTEMPT ready_for_review (H3): the claims to arbitrate.
	Claims []Claim `json:"claims,omitempty"`
}

// Claim is one "AC-n passes, see receipt" assertion in a checkpoint.
type Claim struct {
	AC      string `json:"ac"`
	Receipt string `json:"receipt"`
}

// EventKind names a recorded fact.
type EventKind string

const (
	EvMissionAdmitted   EventKind = "MISSION_ADMITTED"
	EvIntentRevised     EventKind = "INTENT_REVISED"
	EvAttemptStarted    EventKind = "ATTEMPT_STARTED"
	EvAttemptAcked      EventKind = "ATTEMPT_ACKED"
	EvEvidenceRecorded  EventKind = "EVIDENCE_RECORDED"
	EvAttemptFinished   EventKind = "ATTEMPT_FINISHED"
	EvAttemptUnknown    EventKind = "ATTEMPT_UNKNOWN"
	EvAttemptReconciled EventKind = "ATTEMPT_RECONCILED"
	EvCandidateSelected EventKind = "CANDIDATE_SELECTED"
	EvCandidateAccepted EventKind = "CANDIDATE_ACCEPTED"
	EvDelivered         EventKind = "DELIVERED"
	EvWakeRecorded      EventKind = "WAKE_RECORDED"
	EvWakeAcked         EventKind = "WAKE_ACKED"
	EvMissionResumed    EventKind = "MISSION_RESUMED"
	EvEffectSubmitted   EventKind = "EFFECT_SUBMITTED"
	EvEffectUnknown     EventKind = "EFFECT_UNKNOWN"
	EvEffectResubmitted EventKind = "EFFECT_RESUBMITTED"
	EvEffectResolved    EventKind = "EFFECT_RESOLVED"
	EvMissionReleased   EventKind = "MISSION_RELEASED"
	EvAcceptanceRevoked EventKind = "ACCEPTANCE_REVOKED"
	EvClaimRefused      EventKind = "CLAIM_REFUSED" // H3: a ready_for_review claim without current evidence (an alert, R28)
	// Red-team F1: a pre-launch abandonment backs the mission off, and a run
	// of them parks it as BLOCKED.
	EvDispatchDeferred EventKind = "DISPATCH_DEFERRED"
	EvMissionParked    EventKind = "MISSION_PARKED"
)

// knownEventKinds is every kind this build's reducer interprets. A new kind
// is added here together with its Apply case (TestKnownEventKindRegistryIsComplete).
var knownEventKinds = map[EventKind]bool{
	EvMissionAdmitted: true, EvIntentRevised: true, EvAttemptStarted: true, EvAttemptAcked: true,
	EvEvidenceRecorded: true, EvAttemptFinished: true, EvAttemptUnknown: true, EvAttemptReconciled: true,
	EvCandidateSelected: true, EvCandidateAccepted: true, EvDelivered: true, EvWakeRecorded: true,
	EvWakeAcked: true, EvMissionResumed: true, EvEffectSubmitted: true, EvEffectUnknown: true,
	EvEffectResubmitted: true, EvEffectResolved: true, EvMissionReleased: true, EvAcceptanceRevoked: true,
	EvClaimRefused: true, EvDispatchDeferred: true, EvMissionParked: true,
}

// KnownEventKinds lists the kinds this build interprets, sorted.
func KnownEventKinds() []string {
	out := make([]string, 0, len(knownEventKinds))
	for k := range knownEventKinds {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// KnownEventKind reports whether this build can interpret kind k.
func KnownEventKind(k EventKind) bool { return knownEventKinds[k] }

// Event is one recorded fact about a mission. Seq is assigned by the store
// (per-mission, gap-free); Fence is the fence the fact was produced under.
type Event struct {
	Kind    EventKind `json:"kind"`
	Mission MissionID `json:"mission"`
	Seq     int64     `json:"seq,omitempty"`

	Attempt    AttemptID    `json:"attempt,omitempty"`
	Fence      int64        `json:"fence,omitempty"`
	Revision   int64        `json:"revision,omitempty"`
	Generation int64        `json:"generation,omitempty"`
	Head       string       `json:"head,omitempty"`
	Host       HostID       `json:"host,omitempty"`
	Role       string       `json:"role,omitempty"`
	Receipt    string       `json:"receipt,omitempty"`
	Outcome    string       `json:"outcome,omitempty"`
	Prelaunch  bool         `json:"prelaunch,omitempty"`  // ATTEMPT_FINISHED: abandoned before the runner started
	Count      int          `json:"count,omitempty"`      // DISPATCH_DEFERRED / MISSION_PARKED: consecutive pre-launch failures
	BackoffMS  int64        `json:"backoff_ms,omitempty"` // DISPATCH_DEFERRED
	Wait       bool         `json:"wait,omitempty"`       // DISPATCH_DEFERRED: a capacity wait, never counted toward parking
	WaitKey    string       `json:"wait_key,omitempty"`
	Token      string       `json:"token,omitempty"`
	Effect     EffectID     `json:"effect,omitempty"`
	Result     string       `json:"result,omitempty"`
	Submission int          `json:"submission,omitempty"` // EFFECT_UNKNOWN / EFFECT_RESOLVED
	Reason     string       `json:"reason,omitempty"`
	AttemptTo  AttemptState `json:"attempt_to,omitempty"`
	EffectKind string       `json:"effect_kind,omitempty"`
	RequestSHA string       `json:"request_sha,omitempty"`
	Occurrence int          `json:"occurrence,omitempty"`
	Vacuous    bool         `json:"vacuous,omitempty"`
	Claims     []Claim      `json:"claims,omitempty"`
}

// RequestHash is the canonical SHA-256 of a command body. Two commands with
// the same idempotency key must hash equal to be a replay; a different hash is
// IDEMPOTENCY_CONFLICT. encoding/json emits struct fields in declaration order
// with omitempty, so the bytes are deterministic for a given Go build.
// CanonicalRequest is the exact byte form the request hash is taken over:
// the command as JSON with its Request canonicalised (sorted keys). Stored
// with each command row (migration 0011) so the database can verify the hash.
func (c Command) CanonicalRequest() []byte {
	if len(c.Request) > 0 {
		if canon, err := canonicalJSON(c.Request, false); err == nil {
			c.Request = canon
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		// A Command is plain data; Marshal cannot fail. Fail closed anyway.
		return []byte("unmarshalable")
	}
	return b
}

// RequestHash is the hex sha256 of CanonicalRequest.
func (c Command) RequestHash() string {
	sum := sha256.Sum256(c.CanonicalRequest())
	return hex.EncodeToString(sum[:])
}
