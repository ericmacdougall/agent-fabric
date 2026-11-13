package native

import (
	"encoding/json"

	"fmt"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic"
)

// CompactionGrant opts a mission into context compaction (goodqa#15361 N3).
// Absent, a context overflow parks the attempt exactly as before.
type CompactionGrant struct {
	// Mode is "server" (the API compacts inside the request) or "client" (the
	// runner summarises older history and records an append-only row).
	Mode string `json:"mode"`
	// TriggerTokens: server, the API trigger (>= 50000, else its default);
	// client, the estimated input (bytes / 3.5) that starts a compaction
	// (8000..window_tokens; 0: only on overflow).
	TriggerTokens int `json:"trigger_tokens,omitempty"`
	// OutputCap: server, bound on the compaction pass output used in the hold
	// (required); client, max_tokens of the summary request (default 4096).
	OutputCap int `json:"compaction_output_cap,omitempty"`
	// WindowTokens is the model's context window; required for client mode.
	WindowTokens int `json:"window_tokens,omitempty"`
	// KeepPrefix (server) keeps pre-compaction messages in the request.
	KeepPrefix bool `json:"keep_prefix,omitempty"`
	// StripThinking (client) drops thinking blocks from kept assistant
	// messages except the last (the N3-THINKING amendment; default off).
	StripThinking bool `json:"strip_thinking,omitempty"`
}

const (
	serverTriggerMin  = 50_000
	clientTriggerMin  = 8_000
	maxWindowTokens   = 1_000_000
	serverOutputCapMx = 32_768
)

// compactBeta is the one beta header server compaction needs.
const compactBeta = "compact-2026-01-12"

func validateCompaction(g Grant) []string {
	var p []string
	if _, ok := g.Extra["context_management"]; ok {
		p = append(p, "extra.context_management is set by compaction.mode, never by the grant (clear_tool_uses and other edits are refused)")
	}
	for _, b := range g.Betas {
		if !anthropic.AllowedBetas[b] {
			p = append(p, fmt.Sprintf("betas: %q is not on the allowlist", b))
		}
	}
	c := g.Compaction
	if c == nil {
		if len(g.Betas) > 0 {
			p = append(p, "betas needs compaction.mode server")
		}
		return p
	}
	switch c.Mode {
	case agent.CompactServer:
		if c.TriggerTokens != 0 && c.TriggerTokens < serverTriggerMin {
			p = append(p, "compaction.trigger_tokens must be at least 50000 in server mode")
		}
		if c.OutputCap < 1 || c.OutputCap > serverOutputCapMx {
			p = append(p, "compaction.compaction_output_cap is required in server mode (1..32768)")
		}
		if c.WindowTokens != 0 || c.StripThinking {
			p = append(p, "compaction.window_tokens and strip_thinking are client-mode settings")
		}
	case agent.CompactClient:
		if c.WindowTokens < clientTriggerMin || c.WindowTokens > maxWindowTokens {
			p = append(p, "compaction.window_tokens is required in client mode (8000..1000000)")
		}
		if c.TriggerTokens != 0 && (c.TriggerTokens < clientTriggerMin || c.TriggerTokens > c.WindowTokens) {
			p = append(p, "compaction.trigger_tokens must be 8000..window_tokens in client mode")
		}
		if c.OutputCap != 0 && (c.OutputCap < 256 || c.OutputCap > 16_384) {
			p = append(p, "compaction.compaction_output_cap must be 256..16384 in client mode")
		}
		if c.KeepPrefix {
			p = append(p, "compaction.keep_prefix is a server-mode setting")
		}
		if len(g.Betas) > 0 {
			p = append(p, "betas needs compaction.mode server")
		}
	default:
		p = append(p, `compaction.mode must be "server" or "client"`)
	}
	return p
}

// compactionExtra returns the grant's Extra with server compaction's
// context_management added, and the betas the client must send.
func compactionExtra(g Grant) (map[string]json.RawMessage, []string, error) {
	extra := make(map[string]json.RawMessage, len(g.Extra)+1)
	for k, v := range g.Extra {
		extra[k] = v
	}
	if g.Compaction == nil || g.Compaction.Mode != agent.CompactServer {
		return extra, nil, nil
	}
	edit := map[string]any{"type": "compact_20260112", "instructions": agent.ServerInstructions}
	if g.Compaction.TriggerTokens > 0 {
		edit["trigger"] = map[string]any{"type": "input_tokens", "value": g.Compaction.TriggerTokens}
	}
	b, err := json.Marshal(map[string]any{"edits": []any{edit}})
	if err != nil {
		return nil, nil, err
	}
	extra["context_management"] = b
	return extra, []string{compactBeta}, nil
}

func (c *CompactionGrant) agentConfig() agent.CompactionConfig {
	if c == nil {
		return agent.CompactionConfig{}
	}
	return agent.CompactionConfig{Mode: c.Mode, TriggerTokens: c.TriggerTokens, OutputCap: c.OutputCap,
		WindowTokens: c.WindowTokens, KeepPrefix: c.KeepPrefix, StripThinking: c.StripThinking}
}
