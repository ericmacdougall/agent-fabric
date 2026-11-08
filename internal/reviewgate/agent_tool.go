package reviewgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"fabric/internal/agent"
)

// AgentToolConfig binds a wrapped tool to trusted harness identity. OriginalTask
// is the initial user prompt, not a transcript or a later model message.
type AgentToolConfig struct {
	MissionID    string
	SessionID    string
	ProposerID   string
	OriginalTask string
}

// GatedTool submits, evaluates and dispatches every real call through a Gate.
// Its executor is private to this wrapper and cannot be supplied by the model.
type GatedTool struct {
	tool   agent.Tool
	gate   *Gate
	config AgentToolConfig
}

// NewGatedTool builds an isolated deterministic reviewer and real tool executor.
func NewGatedTool(tool agent.Tool, store *PostgresStore, gateConfig Config, config AgentToolConfig) (*GatedTool, error) {
	if tool == nil || store == nil {
		return nil, invalid("wrapped tool and review store are required")
	}
	for name, value := range map[string]string{
		"mission": config.MissionID, "session": config.SessionID,
		"proposer": config.ProposerID, "original task": config.OriginalTask,
	} {
		max := MaxNameBytes
		if name == "original task" {
			max = MaxTaskBytes
		}
		if !validText(value, max) {
			return nil, invalid("invalid %s", name)
		}
	}
	executor := &agentToolExecutor{tool: tool}
	gate, err := NewGate(store, LocalPolicyReviewer{Policy: gateConfig.Policy}, executor, gateConfig)
	if err != nil {
		return nil, err
	}
	return &GatedTool{tool: tool, gate: gate, config: config}, nil
}

func (t *GatedTool) Name() string         { return t.tool.Name() }
func (t *GatedTool) Def() json.RawMessage { return t.tool.Def() }
func (t *GatedTool) SideEffecting() bool  { return t.tool.SideEffecting() }
func (t *GatedTool) Gate() *Gate          { return t.gate }
func (t *GatedTool) toolCallID(ctx context.Context) (agent.ToolCallContext, bool) {
	return agent.ToolCallFromContext(ctx)
}

type agentToolExecutor struct {
	tool agent.Tool
}

func (e *agentToolExecutor) Execute(ctx context.Context, request DispatchRequest) (result ExecutionResult, err error) {
	defer func() {
		if recover() != nil {
			// A panic gives no definite evidence about where the underlying tool
			// stopped. Do not turn it into a safe failure or retry it.
			result = ExecutionResult{Outcome: Unknown}
			err = nil
		}
	}()
	var p Proposal
	if err := json.Unmarshal(request.CanonicalProposal, &p); err != nil {
		return ExecutionResult{Outcome: FailedDefinite}, nil
	}
	canonical, err := CanonicalProposal(p)
	if err != nil || !bytes.Equal(canonical, request.CanonicalProposal) || hashBytes(canonical) != request.RequestHash {
		return ExecutionResult{Outcome: FailedDefinite}, nil
	}
	if p.Tool != e.tool.Name() {
		return ExecutionResult{Outcome: FailedDefinite}, nil
	}
	out, isError := e.tool.Run(ctx, p.Args)
	execution := ExecutionResult{Outcome: Confirmed, Result: &ToolResult{Output: out, IsError: isError}}
	if isError {
		execution.Outcome = FailedDefinite
	}
	return execution, nil
}

// ToolGateResult is deliberately metadata-only: arguments, prompt text and
// reviewer/executor diagnostics are never copied into this control response.
type ToolGateResult struct {
	Status      string      `json:"status"`
	RequestHash string      `json:"request_hash,omitempty"`
	Mode        RolloutMode `json:"mode,omitempty"`
}

func gateToolResult(status, hash string, mode RolloutMode) (string, bool) {
	b, _ := json.Marshal(ToolGateResult{Status: status, RequestHash: hash, Mode: mode})
	return string(b), true
}

func (t *GatedTool) proposal(ctx context.Context, input json.RawMessage) (Proposal, error) {
	call, ok := t.toolCallID(ctx)
	if !ok {
		return Proposal{}, invalid("missing loop-attached tool call identity")
	}
	promptID := "prompt-" + hashBytes([]byte(t.config.OriginalTask))[:24]
	callID := "call-" + strconv.Itoa(call.MessageIndex) + "-" + call.ID
	return Proposal{
		MissionID: t.config.MissionID, SessionID: t.config.SessionID, Tool: t.tool.Name(), Args: input,
		// This placeholder is never trusted: Gate replaces it from StateReader.
		StateFingerprint: "trusted-state-reader",
		Provenance: []ProvenanceHandle{
			{ID: promptID, Origin: OriginUser},
			{ID: callID, Origin: OriginAgent, DerivedFrom: []string{promptID}},
		},
	}, nil
}

func reviewedControl(status, hash string, mode RolloutMode, pause bool) agent.ReviewRunResult {
	out, isError := gateToolResult(status, hash, mode)
	result := agent.ReviewRunResult{Output: out, IsError: isError}
	if pause {
		result.Pause = &agent.ReviewPause{RequestHash: hash, Status: status}
	}
	return result
}

func (t *GatedTool) terminal(ctx context.Context, decision Decision) (agent.ReviewRunResult, error) {
	switch decision.State {
	case StateDenied:
		return reviewedControl("denied", decision.RequestHash, t.gate.mode, false), nil
	case StateUnknown:
		return reviewedControl("unknown_outcome", decision.RequestHash, t.gate.mode, false), nil
	case StateConfirmed, StateFailedDefinite:
		// Durable terminal replay precedes a fresh guard, policy or dispatch
		// decision. Missing legacy output never authorizes another execution.
		stored, err := t.gate.LoadToolResult(ctx, decision.RequestHash)
		if err == nil {
			return agent.ReviewRunResult{Output: stored.Output, IsError: stored.IsError}, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return agent.ReviewRunResult{}, err
		}
		if decision.State == StateConfirmed {
			return reviewedControl("confirmed_result_unavailable", decision.RequestHash, t.gate.mode, false), nil
		}
		return reviewedControl("failed_definite", decision.RequestHash, t.gate.mode, false), nil
	}
	return agent.ReviewRunResult{}, invalid("not a terminal review state: %s", decision.State)
}

func (t *GatedTool) guardAllows(ctx context.Context, input json.RawMessage) (bool, string) {
	call, _ := t.toolCallID(ctx)
	if call.Guard == nil {
		return true, ""
	}
	decision := call.Guard.Check(ctx, t.tool.Name(), input)
	return decision.Allow, decision.Reason
}

// RunReviewed returns control state separately from bytes. New calls pass the
// outer guard before a review request exists. Existing nonterminal requests are
// denied durably if that guard now refuses them; terminal results replay first.
func (t *GatedTool) RunReviewed(ctx context.Context, input json.RawMessage) (agent.ReviewRunResult, error) {
	p, err := t.proposal(ctx, input)
	if err != nil {
		return agent.ReviewRunResult{}, err
	}
	terminal, found, err := t.gate.terminalForCall(ctx, p)
	if err != nil {
		return agent.ReviewRunResult{}, err
	}
	if found {
		return t.terminal(ctx, terminal)
	}
	fresh, err := t.gate.freshProposal(ctx, p)
	if err != nil {
		return agent.ReviewRunResult{}, err
	}
	canonical, err := CanonicalProposal(fresh)
	if err != nil {
		return agent.ReviewRunResult{}, err
	}
	hash := hashBytes(canonical)
	decision, inspectErr := t.gate.Inspect(ctx, hash)
	exists := inspectErr == nil
	if inspectErr != nil && !errors.Is(inspectErr, ErrNotFound) {
		return agent.ReviewRunResult{}, inspectErr
	}
	if exists && (decision.State == StateDenied || decision.State == StateUnknown || decision.State == StateConfirmed || decision.State == StateFailedDefinite) {
		return t.terminal(ctx, decision)
	}
	allowed, reason := t.guardAllows(ctx, input)
	if !allowed {
		if !exists {
			out := "refused by policy: " + reason
			return agent.ReviewRunResult{Output: out, IsError: true, GuardDenied: true}, nil
		}
		if decision.State != StateDispatched {
			decision, err = t.gate.Deny(ctx, decision.RequestHash, "trusted_guard_refusal")
			if err != nil {
				return agent.ReviewRunResult{}, err
			}
			return t.terminal(ctx, decision)
		}
	}
	if !exists {
		decision, err = t.gate.Submit(ctx, t.config.ProposerID, p)
		if err != nil {
			return agent.ReviewRunResult{}, err
		}
	}
	if decision.State == StatePending {
		decision, err = t.gate.Evaluate(ctx, decision.RequestHash, t.config.OriginalTask)
		if err != nil && !errors.Is(err, ErrDenied) {
			return agent.ReviewRunResult{}, err
		}
	}
	switch decision.State {
	case StateDenied, StateUnknown, StateConfirmed, StateFailedDefinite:
		return t.terminal(ctx, decision)
	case StateAwaitingHuman:
		return reviewedControl("approval_required", decision.RequestHash, t.gate.mode, true), nil
	case StateDispatched:
		outcome, recoverErr := t.gate.Recover(ctx, decision.RequestHash)
		if recoverErr != nil {
			return agent.ReviewRunResult{}, recoverErr
		}
		if outcome == Unknown {
			return t.terminal(ctx, Decision{RequestHash: decision.RequestHash, State: StateUnknown})
		}
		if outcome == Dispatched {
			return reviewedControl("dispatch_in_progress", decision.RequestHash, t.gate.mode, true), nil
		}
		decision.State = RequestState(outcome)
		return t.terminal(ctx, decision)
	}

	// The request hash itself is stable across process replay and is already
	// unique in the durable dispatch-key namespace.
	dispatched, err := t.gate.Dispatch(ctx, decision.RequestHash, p, "review-gate-"+decision.RequestHash)
	if errors.Is(err, ErrApprovalRequired) {
		return reviewedControl("approval_required", decision.RequestHash, t.gate.mode, true), nil
	}
	if errors.Is(err, ErrUnknown) || dispatched.Outcome == Unknown {
		return t.terminal(ctx, Decision{RequestHash: decision.RequestHash, State: StateUnknown})
	}
	if errors.Is(err, ErrStale) {
		return reviewedControl("stale_state", decision.RequestHash, t.gate.mode, true), nil
	}
	if err != nil {
		return agent.ReviewRunResult{}, err
	}
	if t.gate.mode == Shadow || t.gate.mode == Evaluate {
		return reviewedControl(string(t.gate.mode)+"_no_execution", decision.RequestHash, t.gate.mode, false), nil
	}
	decision.State = RequestState(dispatched.Outcome)
	return t.terminal(ctx, decision)
}

// Run retains the ordinary Tool surface for direct callers. The agent loop uses
// RunReviewed, so no byte sequence returned here is interpreted as control.
func (t *GatedTool) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	result, err := t.RunReviewed(ctx, input)
	if err != nil {
		out, _ := gateToolResult("review_failed", "", t.gate.mode)
		return out, true
	}
	return result.Output, result.IsError
}

var _ agent.Tool = (*GatedTool)(nil)
var _ agent.DurableReviewTool = (*GatedTool)(nil)
var _ Executor = (*agentToolExecutor)(nil)
