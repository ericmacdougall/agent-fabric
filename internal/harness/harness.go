// Package harness is the adapter seam between Fabric and an agent harness
// (the built-in native loop, an interactive Claude Code session, Codex, and
// later others). It is a pure package: it knows nothing about Postgres, the
// reducer or a specific harness. Adapters push typed events into a Sink; how
// those events are persisted is the sink's concern (a thin sink maps them to
// the F1 envelope once it lands, see docs/adr/0001-kernel-scope.md).
//
// Contract every adapter honours, enforced by harnesstest.Conformance:
//
//   - Events of one session have Seq 1, 2, 3, ... with no gap.
//   - Exactly one KindFinal event is emitted, and it is the last event.
//   - A harness event the adapter cannot classify becomes KindUnknown; it never
//     crashes the session and never is dropped silently.
//   - Every string leaving an adapter passes the Emitter's redactor.
//   - A harness that ends without reporting a result settles as failed with
//     its exit status, never as success.
//   - Input to a non-interactive harness returns ErrNotInteractive; Input
//     after the session ended returns ErrSessionEnded; Input during a running
//     turn is queued in order and never lost silently.
//   - Interrupt and Stop are idempotent.
package harness

import (
	"context"
	"encoding/json"
	"errors"
)

// EventVersion is the version of the Event shape and payloads below.
const EventVersion = 1

// Kind is the type of an Event.
type Kind string

const (
	KindTurn     Kind = "turn"
	KindToolCall Kind = "tool_call"
	KindMessage  Kind = "message"
	KindUsage    Kind = "usage"
	KindFinal    Kind = "final"
	KindUnknown  Kind = "unknown"
)

// Event is one thing a harness did. Payload is exactly one of the typed
// structs below for the Kind, encoded as JSON, so a sink can store it in one
// nested key (ADR interim rule) without the flat event struct growing.
type Event struct {
	Version int             `json:"v"`
	Kind    Kind            `json:"kind"`
	Harness string          `json:"harness"`
	Session string          `json:"session"`
	Seq     int64           `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

// Turn starts a model turn.
type Turn struct {
	Index int `json:"index"`
}

// ToolCall records a tool the harness ran or asked to run. Only a digest of
// the input is carried; the input itself stays in the harness.
type ToolCall struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	InputSHA256 string `json:"input_sha256"`
	Status      string `json:"status"` // requested | ok | error | denied
}

// Message is human-readable text from or to the model.
type Message struct {
	Role string `json:"role"` // assistant | user | system
	Text string `json:"text"`
}

// Usage is token usage for one turn.
type Usage struct {
	Input       int64 `json:"input_tokens"`
	Output      int64 `json:"output_tokens"`
	CacheCreate int64 `json:"cache_creation_input_tokens"`
	CacheRead   int64 `json:"cache_read_input_tokens"`
}

// Outcome values of Final.
const (
	OutcomeDone        = "done"
	OutcomeFailed      = "failed"
	OutcomeInterrupted = "interrupted"
	OutcomeStopped     = "stopped"
)

// Final ends a session. ExitCode is -1 when the harness has no process.
type Final struct {
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// Unknown carries a harness event the adapter could not classify. Raw is
// truncated and redacted.
type Unknown struct {
	Why string `json:"why"`
	Raw string `json:"raw,omitempty"`
}

// Caps are what an adapter can do. Callers branch on these and on nothing
// else about a harness.
type Caps struct {
	Resume      bool `json:"resume"`
	Interactive bool `json:"interactive"`
	Subagents   bool `json:"subagents"`
}

// Sink receives a session's events in order. Emit must not block for long;
// an error from Emit is recorded by the Emitter and ends the session as
// failed (fail closed: an event that cannot be recorded is not skipped).
type Sink interface {
	Emit(ctx context.Context, e Event) error
}

// StartSpec describes one session to start. Env is the complete allow-listed
// environment; adapters never inherit the caller's.
type StartSpec struct {
	SessionID string
	Prompt    string
	Dir       string
	Env       map[string]string
	Model     string
	// Extra is adapter-specific configuration, JSON, validated by the adapter.
	Extra json.RawMessage
}

// Session is one running harness conversation.
type Session interface {
	ID() string
	// Input sends a user message to a running interactive session.
	Input(ctx context.Context, msg string) error
	// Interrupt stops the current turn. An interactive session stays usable.
	// A non-interactive session ends with OutcomeInterrupted.
	Interrupt(ctx context.Context) error
	// Stop ends the session. The Final outcome is OutcomeStopped unless the
	// session had already ended.
	Stop(ctx context.Context) error
	// Wait blocks until the Final event has been emitted and returns it.
	Wait(ctx context.Context) (Final, error)
}

// Adapter starts sessions of one harness.
type Adapter interface {
	Name() string
	Caps() Caps
	Start(ctx context.Context, spec StartSpec, sink Sink) (Session, error)
}

// Errors returned by sessions.
var (
	ErrNotInteractive = errors.New("harness: session is not interactive")
	ErrSessionEnded   = errors.New("harness: session has ended")
)
