package agent

import "context"

// ToolCallContext is the durable identity of the provider tool call being
// executed. It is attached by Loop, never parsed from model-controlled input.
type ToolCallContext struct {
	ID           string
	MessageIndex int
	// Guard is attached by Loop, not supplied in model-controlled tool input.
	// Reviewed tools use it only for nonterminal/new work; durable terminal
	// results replay before another policy decision can replace them.
	Guard Guard
}

type toolCallContextKey struct{}

// WithToolCallContext binds a real, already-checkpointed tool call to ctx.
func WithToolCallContext(ctx context.Context, call ToolCallContext) context.Context {
	return context.WithValue(ctx, toolCallContextKey{}, call)
}

// ToolCallFromContext returns the loop-attached identity, if present.
func ToolCallFromContext(ctx context.Context) (ToolCallContext, bool) {
	call, ok := ctx.Value(toolCallContextKey{}).(ToolCallContext)
	return call, ok && call.ID != ""
}
