package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	WaitKey    string    `json:"wait_key,omitempty"`
	Token      string    `json:"token,omitempty"`
	Effect     EffectID  `json:"effect,omitempty"`
	Result     string    `json:"result,omitempty"` // RECONCILE_ATTEMPT / RESOLVE_EFFECT
	Reason     string    `json:"reason,omitempty"`

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
)

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
	WaitKey    string       `json:"wait_key,omitempty"`
	Token      string       `json:"token,omitempty"`
	Effect     EffectID     `json:"effect,omitempty"`
	Result     string       `json:"result,omitempty"`
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
func (c Command) RequestHash() string {
	if len(c.Request) > 0 {
		if canon, err := CanonicalJSON(c.Request); err == nil {
			c.Request = canon
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		// A Command is plain data; Marshal cannot fail. Fail closed anyway.
		return "unmarshalable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
