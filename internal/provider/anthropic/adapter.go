package anthropic

import (
	"context"
	"encoding/json"
	"errors"

	"fabric/internal/agent"
)

// Provider adapts Client to agent.Provider.
type Provider struct{ C *Client }

var _ agent.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return "anthropic" }

func (p *Provider) Caps() agent.Caps {
	return agent.Caps{ResultsInUserMessage: true, SignedReasoning: true}
}

func (p *Provider) UserTextBlock(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return b
}

func (p *Provider) ToolResultBlock(id, text string, isErr bool) json.RawMessage {
	m := map[string]any{"type": "tool_result", "tool_use_id": id, "content": text}
	if isErr {
		m["is_error"] = true
	}
	b, _ := json.Marshal(m)
	return b
}

func (p *Provider) Send(ctx context.Context, req agent.Request, h agent.Hooks) (*agent.Result, error) {
	spec := RequestSpec{Model: req.Model, MaxTokens: req.MaxTokens, System: req.System, Tools: req.Tools, Extra: req.Extra}
	for _, m := range req.Messages {
		spec.Messages = append(spec.Messages, Msg{Role: m.Role, Content: m.Content})
	}
	body, err := BuildRequest(spec)
	if err != nil {
		return nil, err
	}
	hk := Hooks{OnEvent: h.OnEvent}
	if h.BeforeAttempt != nil {
		hk.BeforeAttempt = func(n int) error { return h.BeforeAttempt(n, len(body)) }
	}
	if h.AfterAttempt != nil {
		hk.AfterAttempt = func(r AttemptRecord) { h.AfterAttempt(toAgentAttempt(r)) }
	}
	msg, recs, err := p.C.Send(ctx, body, hk)
	if err != nil {
		var pk *ParkError
		if errors.As(err, &pk) {
			return nil, &agent.ParkError{Reason: pk.Reason}
		}
		return nil, err
	}
	res := &agent.Result{
		Content: msg.Content, StopReason: msg.StopReason, Model: msg.Model, RequestSHA: RequestSHA(body),
		Usage: agent.Usage{Input: msg.Usage.Input, Output: msg.Usage.Output, CacheCreate: msg.Usage.CacheCreate,
			CacheRead: msg.Usage.CacheRead, ServerToolUses: msg.Usage.ServerTools},
	}
	switch ClassifyStop(msg.StopReason) {
	case StopEnd:
		res.Stop = agent.StopEnd
	case StopTools:
		res.Stop = agent.StopTools
	case StopMaxTokens:
		res.Stop = agent.StopMaxTokens
	case StopPause:
		res.Stop = agent.StopPause
	case StopRefusal:
		res.Stop = agent.StopRefusal
	case StopContext:
		res.Stop = agent.StopContext
	default:
		res.Stop = agent.StopUnknown
	}
	for _, r := range recs {
		res.Attempts = append(res.Attempts, toAgentAttempt(r))
		if r.RequestID != "" {
			res.RequestIDs = append(res.RequestIDs, r.RequestID)
		}
	}
	tus, err := msg.ToolUses()
	if err != nil {
		return nil, err
	}
	for _, t := range tus {
		res.ToolUses = append(res.ToolUses, agent.ToolUse{ID: t.ID, Name: t.Name, Input: t.Input})
	}
	return res, nil
}

func toAgentAttempt(r AttemptRecord) agent.AttemptRecord {
	a := agent.AttemptRecord{N: r.N, Status: r.Status, RequestID: r.RequestID, Outcome: r.Outcome, MaybeBilled: r.MaybeBilled, Err: r.Err}
	if r.Usage != nil {
		a.Usage = &agent.Usage{Input: r.Usage.Input, Output: r.Usage.Output, CacheCreate: r.Usage.CacheCreate, CacheRead: r.Usage.CacheRead, ServerToolUses: r.Usage.ServerTools}
	}
	return a
}
