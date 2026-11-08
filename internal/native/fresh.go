package native

import (
	"context"
	"encoding/json"
	"fmt"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/provider/anthropic"
	"fabric/internal/store"
)

// nativeProvider revalidates the fresh lease before every provider send and
// selects the current database month before every HTTP attempt, retries
// included. Settlement locks the reservation resources captured by the hold,
// so a month transition cannot settle against the wrong advisory lock.
type nativeProvider struct {
	base    *anthropic.Provider
	st      *store.Store
	ledger  *agent.Ledger
	attempt domain.AttemptID
	fence   int64
}

func (p *nativeProvider) Name() string                           { return p.base.Name() }
func (p *nativeProvider) Caps() agent.Caps                       { return p.base.Caps() }
func (p *nativeProvider) UserTextBlock(s string) json.RawMessage { return p.base.UserTextBlock(s) }
func (p *nativeProvider) ToolResultBlock(id, text string, isErr bool) json.RawMessage {
	return p.base.ToolResultBlock(id, text, isErr)
}
func (p *nativeProvider) Send(ctx context.Context, req agent.Request, hooks agent.Hooks) (*agent.Result, error) {
	if err := p.st.HeartbeatLive(ctx, p.attempt, p.fence); err != nil {
		return nil, err
	}
	before := hooks.BeforeAttempt
	hooks.BeforeAttempt = func(n int, bodyBytes int) error {
		month, err := monthSource(ctx, p.st)
		if err != nil {
			return fmt.Errorf("native: month key: %w", err)
		}
		p.ledger.Caps[0].Resource = agent.SpendResource("month", month)
		if before != nil {
			return before(n, bodyBytes)
		}
		return nil
	}
	return p.base.Send(ctx, req, hooks)
}

// freshTool is the final local boundary before a host process can start.
type freshTool struct {
	agent.Tool
	st      *store.Store
	attempt domain.AttemptID
	fence   int64
}

func (t freshTool) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	if err := t.st.HeartbeatLive(ctx, t.attempt, t.fence); err != nil {
		return "attempt lease is stale or expired; tool not run", true
	}
	return t.Tool.Run(ctx, input)
}
