// Package claude is the fabric's adapter for Claude Code's headless mode
// (DESIGN.md 3.6, probe E1). It parses `claude -p --output-format stream-json
// --verbose` output into identity, usage and an end-of-run classification,
// and builds the launch line from a grant. It never names a model (R23).
package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxLineBytes bounds one stream event (R25: bounded parsing of untrusted JSONL).
const MaxLineBytes = 4 << 20

// Usage is the per-turn and cumulative token accounting from the stream.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

// Add accumulates another turn's usage.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.OutputTokens += o.OutputTokens
}

// Context is the prompt size the model saw on a turn: everything read or
// written to the cache plus the uncached input.
func (u Usage) Context() int64 { return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens }

// EndKind classifies how a run ended (DESIGN.md 3.4).
type EndKind string

const (
	EndCheckpoint  EndKind = "checkpoint" // result with a structured output
	EndAbandoned   EndKind = "abandoned"  // result without the structured output the grant demanded
	EndFailed      EndKind = "failed"     // result with is_error / a non-success subtype
	EndUnknown     EndKind = "unknown"    // the stream ended without a result event
	EndInterrupted EndKind = "interrupted"
)

// Run is what the adapter reports to the daemon for one worker process.
type Run struct {
	SessionID        string
	Model            string // as reported by system/init; compared with the grant, never chosen here
	ClaudeVersion    string
	ToolCount        int
	MCPServers       int
	Plugins          int
	Events           int
	Turns            int64
	Usage            Usage // sum over assistant turns
	LastTurn         Usage // the final assistant turn (the per-turn context floor, AC-13)
	ResultUsage      Usage
	TotalCostUSD     float64
	StopReason       string
	ResultSubtype    string
	IsError          bool
	PermissionDenied int
	StructuredOutput json.RawMessage
	ResultUUID       string
	APIRetries       int
	RateLimited      bool
	End              EndKind
	ParseErrors      []string
	// Contradictions are identity or ordering faults (goodqa#15082): a
	// second init, a result before init, init after a result, a second
	// result, a changed session id. The daemon never verifies a run that
	// has any, whatever its last event says.
	Contradictions []string
}

type envelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	UUID      string `json:"uuid"`
	// system/init
	Model             string            `json:"model"`
	Tools             []json.RawMessage `json:"tools"`
	MCPServers        []json.RawMessage `json:"mcp_servers"`
	Plugins           []json.RawMessage `json:"plugins"`
	ClaudeCodeVersion string            `json:"claude_code_version"`
	// assistant
	Message *struct {
		Usage Usage `json:"usage"`
	} `json:"message"`
	// result
	NumTurns          int64             `json:"num_turns"`
	TotalCostUSD      float64           `json:"total_cost_usd"`
	Usage             *Usage            `json:"usage"`
	IsError           bool              `json:"is_error"`
	StopReason        string            `json:"stop_reason"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	// system/api_retry
	Error string `json:"error"`
}

// Parse reads a stream-json run to its end. expectStructured says the grant
// demanded a checkpoint schema, so a result without structured_output is
// EndAbandoned. A malformed or oversized line is recorded, not fatal: the run
// is then classified EndUnknown unless a well-formed result arrives later.
func Parse(r io.Reader, expectStructured bool) (Run, error) {
	run := Run{End: EndUnknown}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxLineBytes)
	sawResult := false
	inits := 0
	contradict := func(format string, a ...any) {
		run.Contradictions = append(run.Contradictions, fmt.Sprintf("event %d: ", run.Events)+fmt.Sprintf(format, a...))
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		run.Events++
		var e envelope
		if err := json.Unmarshal(line, &e); err != nil {
			run.ParseErrors = append(run.ParseErrors, fmt.Sprintf("event %d: %v", run.Events, err))
			continue
		}
		if e.SessionID != "" {
			if run.SessionID == "" {
				run.SessionID = e.SessionID
			} else if run.SessionID != e.SessionID {
				run.ParseErrors = append(run.ParseErrors, fmt.Sprintf("event %d: session id changed %s -> %s", run.Events, run.SessionID, e.SessionID))
				contradict("session id changed %s -> %s", run.SessionID, e.SessionID)
			}
		}
		switch e.Type {
		case "system":
			switch e.Subtype {
			case "init":
				inits++
				if sawResult {
					contradict("init after a result")
				}
				if inits > 1 {
					// The first init's identity stands; a later one never
					// replaces it (goodqa#15082).
					contradict("second init (model %q, first %q)", e.Model, run.Model)
					continue
				}
				run.Model = e.Model
				run.ToolCount = len(e.Tools)
				run.MCPServers = len(e.MCPServers)
				run.Plugins = len(e.Plugins)
				run.ClaudeVersion = e.ClaudeCodeVersion
			case "api_retry":
				run.APIRetries++
			}
		case "assistant":
			if e.Message != nil {
				run.Usage.Add(e.Message.Usage)
				run.LastTurn = e.Message.Usage
			}
		case "rate_limit_event":
			run.RateLimited = true
		case "result":
			if inits == 0 {
				contradict("result before init")
			}
			if sawResult {
				// A later result never hides an earlier one: the run keeps the
				// worst end seen (below) and is marked contradictory.
				contradict("second result")
			}
			prevFailed := sawResult && run.End == EndFailed
			sawResult = true
			run.ResultSubtype = e.Subtype
			run.Turns = e.NumTurns
			run.TotalCostUSD = e.TotalCostUSD
			run.IsError = e.IsError
			run.StopReason = e.StopReason
			run.PermissionDenied = len(e.PermissionDenials)
			run.ResultUUID = e.UUID
			if e.Usage != nil {
				run.ResultUsage = *e.Usage
			}
			run.StructuredOutput = e.StructuredOutput
			switch {
			case prevFailed || e.IsError || e.Subtype != "success":
				run.End = EndFailed
			case expectStructured && len(e.StructuredOutput) == 0:
				run.End = EndAbandoned
			default:
				run.End = EndCheckpoint
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			run.ParseErrors = append(run.ParseErrors, "event exceeds MaxLineBytes")
		} else {
			return run, err
		}
	}
	if !sawResult {
		run.End = EndUnknown
	}
	return run, nil
}
