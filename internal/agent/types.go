// Package agent is Fabric's native agent loop: it drives a provider through
// tool-using turns, checkpoints every turn to Postgres under the attempt
// fence, holds budget before any byte is sent, and never re-runs a
// side-effecting tool after a crash. It knows nothing about a specific
// provider beyond the Provider interface and its capability flags.
package agent

import (
	"context"
	"encoding/json"
)

// Usage is provider-neutral token usage.
type Usage struct {
	Input          int64 `json:"input_tokens"`
	Output         int64 `json:"output_tokens"`
	CacheCreate    int64 `json:"cache_creation_input_tokens"`
	CacheRead      int64 `json:"cache_read_input_tokens"`
	ServerToolUses int64 `json:"server_tool_uses"`
}

// Stop classifies why a response ended.
type Stop int

const (
	StopUnknown Stop = iota
	StopEnd
	StopTools
	StopMaxTokens
	StopPause
	StopRefusal
	StopContext
)

// Msg is one conversation message; Content blocks are raw provider JSON.
type Msg struct {
	Role    string
	Content []json.RawMessage
}

// Request is what the loop asks a provider to send.
type Request struct {
	Model     string
	MaxTokens int
	System    string
	Tools     []json.RawMessage
	Messages  []Msg
	Extra     map[string]json.RawMessage
}

// AttemptRecord describes one HTTP attempt of one request.
type AttemptRecord struct {
	N           int
	Status      int
	RequestID   string
	Outcome     string // ok | http_error | stream_error | truncated | transport | protocol
	MaybeBilled bool
	Usage       *Usage
	Err         string
}

// Hooks run around every attempt of a Send, retries included.
type Hooks struct {
	// BeforeAttempt runs before any byte of attempt n is sent; bodyBytes is the
	// exact request size. A returned error aborts the send.
	BeforeAttempt func(n int, bodyBytes int) error
	AfterAttempt  func(AttemptRecord)
	// OnEvent is a test seam called per stream event (crash injection).
	OnEvent func(n int)
}

// Result is a completed response.
type Result struct {
	Content    []json.RawMessage
	StopReason string
	Stop       Stop
	Usage      Usage
	Model      string
	RequestIDs []string
	RequestSHA string
	Attempts   []AttemptRecord
	ToolUses   []ToolUse
}

// ToolUse is a tool call the model requested.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Caps are the capability flags the loop may branch on (and nothing else).
type Caps struct {
	// ResultsInUserMessage: tool results travel as blocks in a user message.
	ResultsInUserMessage bool
	// SignedReasoning: reasoning blocks are signed and must be replayed unchanged.
	SignedReasoning bool
}

// Provider is the one seam between the loop and a model API.
type Provider interface {
	Name() string
	Caps() Caps
	Send(ctx context.Context, req Request, h Hooks) (*Result, error)
	// ToolResultBlock renders one tool result for the next user message.
	ToolResultBlock(toolUseID, text string, isError bool) json.RawMessage
	// UserTextBlock renders the initial prompt.
	UserTextBlock(text string) json.RawMessage
}

// ParkError means the mission must stop with a human-readable reason.
type ParkError struct{ Reason string }

func (e *ParkError) Error() string { return "park: " + e.Reason }

// Tool is one callable capability.
type Tool interface {
	Name() string
	Def() json.RawMessage
	// SideEffecting tools are never re-run after a crash: an unresolved call
	// gets a synthetic "interrupted, effects unknown" result instead.
	SideEffecting() bool
	Run(ctx context.Context, input json.RawMessage) (out string, isError bool)
}

// InterruptNoter is implemented by side-effecting tools that can add facts to
// the INTERRUPTED text of a call found EXECUTING on resume (for example the
// file's current digest next to its recorded pre-image). It also lets the tool
// remove what that call left behind. It must not execute the call again.
type InterruptNoter interface {
	InterruptedNote(callID string, input json.RawMessage) string
}

// ReviewPause is trusted control metadata returned by a DurableReviewTool.
// Ordinary tool output is always data, even when it contains the same JSON.
type ReviewPause struct {
	RequestHash string `json:"request_hash"`
	Status      string `json:"status"`
}

// ReviewRunResult separates durable control outcomes from ordinary stdout.
// GuardDenied uses the ordinary PENDING -> EXECUTING -> DONE checkpoint path
// because no review request owns execution. Other completed outcomes require
// their matching durable review outcome.
type ReviewRunResult struct {
	Output      string
	IsError     bool
	Pause       *ReviewPause
	GuardDenied bool
}

// DurableReviewTool keeps its own execution authority. The loop leaves its
// checkpointed call PENDING while review or dispatch is unresolved; a crash
// during dispatch is resolved by that authority, never by running bytes again.
type DurableReviewTool interface {
	Tool
	RunReviewed(ctx context.Context, input json.RawMessage) (ReviewRunResult, error)
}

// Decision is a guard verdict.
type Decision struct {
	Allow  bool
	Reason string
}

// Guard decides before a tool runs. Authority comes only from the guard's own
// store-backed context, never from anything in the tool input.
type Guard interface {
	Check(ctx context.Context, tool string, input json.RawMessage) Decision
}

// Redactor scrubs secret shapes from tool output at creation.
type Redactor interface{ Redact(s string) string }

// Billing modes and cost labels (AC-27).
const (
	BillingAPIKey       = "api_key"
	BillingSubscription = "subscription"
	LabelAPIEstimate    = "billed_estimate_from_usage"
	LabelNotional       = "notional_cli_reported"
)

// Outcomes of a run.
const (
	OutcomeDone             = "done"
	OutcomeParked           = "parked"
	OutcomeApprovalRequired = "approval_required" // resumable: no terminal DB outcome
	OutcomeBudgetExhausted  = "budget_exhausted"
	OutcomeMaxTurns         = "max_turns"
	OutcomeWallExhausted    = "wall_exhausted"
)
