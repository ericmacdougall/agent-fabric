// Package reviewgate implements default-deny review and fenced execution of
// agent-proposed tool calls. Its policy and canonicalization code is pure Go;
// PostgreSQL is the durable authority for approvals, dispatch and audit.
//
// Provenance handles and human operator identities remain trusted-broker
// inputs: the broker must authenticate their origin and must not accept agent
// self-attestation. The package checks their structure and separation, but
// cannot independently authenticate external people or data provenance.
package reviewgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxProposalBytes is deliberately below one megabyte. It covers the
	// complete canonical proposal, not merely the tool arguments.
	MaxProposalBytes      = (1 << 20) - 1
	MaxTaskBytes          = 64 * 1024
	MaxAuditText          = 1024
	MaxNameBytes          = 256
	MaxDurableResultBytes = 64 * 1024
	PolicyV1              = "review-gate/v1"
)

var (
	ErrInvalid          = errors.New("review gate: invalid input")
	ErrDenied           = errors.New("review gate: denied")
	ErrNotFound         = errors.New("review gate: not found")
	ErrConflict         = errors.New("review gate: conflict")
	ErrStale            = errors.New("review gate: stale request")
	ErrApprovalRequired = errors.New("review gate: human approval required")
	ErrCircuitOpen      = errors.New("review gate: denial circuit open")
	ErrUnknown          = errors.New("review gate: unknown outcome requires settlement")
	ErrNotDispatchOwner = errors.New("review gate: dispatch already owned")
)

// RolloutMode controls whether an otherwise dispatchable request executes.
type RolloutMode string

const (
	Shadow   RolloutMode = "shadow"
	Evaluate RolloutMode = "evaluate"
	Enforce  RolloutMode = "enforce"
)

// RiskTier is ordered. A model may allow only tiers at or below ModelMaxTier.
type RiskTier int

const (
	TierReadOnly RiskTier = 0
	TierLow      RiskTier = 1
	TierHigh     RiskTier = 2
	TierCritical RiskTier = 3
)

type Reversibility string

const (
	NoSideEffect Reversibility = "no_side_effect"
	Reversible   Reversibility = "reversible"
	UnknownUndo  Reversibility = "unknown"
	Irreversible Reversibility = "irreversible"
)

// ProvenanceOrigin is structural metadata supplied by the caller. Tool,
// external and agent origins are intrinsically tainted; Tainted=false cannot
// launder them. DerivedFrom forms a checked, acyclic provenance graph.
type ProvenanceOrigin string

const (
	OriginUser     ProvenanceOrigin = "user"
	OriginOperator ProvenanceOrigin = "operator"
	OriginTool     ProvenanceOrigin = "tool"
	OriginExternal ProvenanceOrigin = "external"
	OriginAgent    ProvenanceOrigin = "agent"
)

type ProvenanceHandle struct {
	ID          string           `json:"id"`
	Origin      ProvenanceOrigin `json:"origin"`
	Tainted     bool             `json:"tainted,omitempty"`
	DerivedFrom []string         `json:"derived_from,omitempty"`
}

// Proposal is the complete request-hash subject. Original task text, model
// transcripts, operator identity and idempotency keys are intentionally not
// hash inputs. Args must be a JSON object.
type Proposal struct {
	MissionID        string             `json:"mission_id"`
	SessionID        string             `json:"session_id"`
	Tool             string             `json:"tool"`
	Args             json.RawMessage    `json:"args"`
	StateFingerprint string             `json:"state_fingerprint"`
	Provenance       []ProvenanceHandle `json:"provenance"`
}

// RequestHash returns the lowercase SHA-256 of CanonicalProposal bytes.
func RequestHash(p Proposal) (string, error) {
	c, err := CanonicalProposal(p)
	if err != nil {
		return "", err
	}
	return hashBytes(c), nil
}

// RiskAssessment is computed from versioned, content-digested policy and
// checked provenance, never accepted from the model.
type RiskAssessment struct {
	Tier          RiskTier      `json:"tier"`
	Reversibility Reversibility `json:"reversibility"`
	Tainted       bool          `json:"tainted"`
	SideEffect    bool          `json:"side_effect"`
	HumanRequired bool          `json:"human_required"`
	PolicyVersion string        `json:"policy_version"`
	PolicyDigest  string        `json:"policy_digest"`
}

// StateReader is a trusted, fresh world-state authority. Enforce mode requires
// it and ignores caller-supplied fingerprints at Submit, Evaluate and Dispatch.
// Dispatch checks again immediately before the executor call. External state
// still cannot be made atomic with execution after that final check; callers
// need versioned state and executor-side conditional writes where available.
type StateReader interface {
	CurrentFingerprint(context.Context, string, string) (string, error)
}

// ReviewInput is the model's entire view. In particular there is no agent
// transcript, tool history, approval API or executor capability here.
type ReviewInput struct {
	OriginalTask      string          `json:"original_task"`
	CanonicalProposal json.RawMessage `json:"canonical_proposal"`
	Risk              RiskAssessment  `json:"risk"`
}

type ReviewVerdict string

const (
	ReviewAllow    ReviewVerdict = "allow"
	ReviewDeny     ReviewVerdict = "deny"
	ReviewEscalate ReviewVerdict = "escalate"
)

type ModelDecision struct {
	Verdict ReviewVerdict
	Reason  string
}

// ModelReviewer may deny or escalate any request. Allow is effective only
// when deterministic policy says no human is required.
type ModelReviewer interface {
	Review(context.Context, ReviewInput) (ModelDecision, error)
}

type Outcome string

const (
	Dispatched     Outcome = "DISPATCHED"
	Confirmed      Outcome = "CONFIRMED"
	FailedDefinite Outcome = "FAILED_DEFINITE"
	Unknown        Outcome = "UNKNOWN"
)

// DispatchRequest contains no task or transcript. Fence and idempotency key
// let an executor suppress duplicate business effects without deciding trust.
//
// Distributed executor fencing contract: the executor must durably deduplicate
// IdempotencyKey, reject a fence after it has been superseded or settled
// UNKNOWN, and honor cancellation. PostgreSQL cannot retract an external side
// effect from a partitioned executor that ignores this contract; such an
// effect is resolved only by readback/operator settlement.
type DispatchRequest struct {
	RequestHash       string          `json:"request_hash"`
	CanonicalProposal json.RawMessage `json:"canonical_proposal"`
	IdempotencyKey    string          `json:"idempotency_key"`
	Fence             int64           `json:"fence"`
}

// ToolResult is the executor's transient return value. Gate redacts and bounds
// it before storage; raw bytes are never written to review-gate tables.
type ToolResult struct {
	Output  string
	IsError bool
}

type ExecutionResult struct {
	Outcome Outcome
	Detail  string      // diagnostic only: discarded, never copied into audit storage
	Result  *ToolResult // optional durable return value for terminal dispatch
}

// StoredToolResult is the bounded, redacted value safe to replay to the agent.
type StoredToolResult struct {
	Output    string
	IsError   bool
	Truncated bool
}

// Redactor is the same secret-scrubbing boundary used for agent tool output.
type Redactor interface {
	Redact(string) string
}

// Executor errors are ambiguous by default. Only an explicit CONFIRMED or
// FAILED_DEFINITE result without an error settles directly.
type Executor interface {
	Execute(context.Context, DispatchRequest) (ExecutionResult, error)
}

type RequestState string

const (
	StatePending        RequestState = "PENDING"
	StateDenied         RequestState = "DENIED"
	StateReady          RequestState = "READY"
	StateAwaitingHuman  RequestState = "AWAITING_HUMAN"
	StateApproved       RequestState = "APPROVED"
	StateDispatched     RequestState = "DISPATCHED"
	StateUnknown        RequestState = "UNKNOWN"
	StateConfirmed      RequestState = "CONFIRMED"
	StateFailedDefinite RequestState = "FAILED_DEFINITE"
)

type Decision struct {
	RequestHash string         `json:"request_hash"`
	State       RequestState   `json:"state"`
	Mode        RolloutMode    `json:"mode"`
	Risk        RiskAssessment `json:"risk"`
	Reason      string         `json:"reason,omitempty"`
	Replayed    bool           `json:"replayed,omitempty"`
}

type DispatchResult struct {
	Decision Decision `json:"decision"`
	Outcome  Outcome  `json:"outcome"`
	Owner    bool     `json:"owner"` // true only for the caller that owned the executor call
}

type Approval struct {
	RequestHash string
	OperatorID  string
	ExpiresAt   time.Time
	Reason      string // only its digest is audited; the text is never returned
}

// Rejection is a durable human denial. Actor is authenticated by the trusted
// broker and must differ from the agent proposer; Reason is not persisted.
type Rejection struct {
	RequestHash string
	Actor       string
	Reason      string
}

// Settlement is the only exit from UNKNOWN. Evidence is hashed for audit; its
// potentially sensitive text is not stored.
type Settlement struct {
	RequestHash string
	Fence       int64
	Outcome     Outcome
	Actor       string
	Evidence    string
}

func validMode(m RolloutMode) bool { return m == Shadow || m == Evaluate || m == Enforce }

func validText(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && len(s) <= max && !strings.ContainsRune(s, '\x00')
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func boundedReason(s string) string {
	if len(s) > MaxAuditText {
		s = s[:MaxAuditText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
