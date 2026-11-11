package claudecode

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// handleFixed is a whitelist reducer. Provider prose, hook output, unknown
// frames, permission suggestions and tool arguments never enter the journal.
func (s *session) handleFixed(line []byte) {
	f := s.a.FixedWrite
	fail := func(reason fixedFailure) { f.refuseAs(reason); _ = s.proc.Kill() }
	s.fixedBytes += len(line)
	if s.fixedBytes > 8<<20 {
		fail(fixedProtocol)
		return
	}
	var w struct {
		Type            string            `json:"type"`
		Subtype         string            `json:"subtype"`
		SessionID       string            `json:"session_id"`
		ParentToolUseID *string           `json:"parent_tool_use_id"`
		AgentID         string            `json:"agent_id"`
		Tools           []string          `json:"tools"`
		MCP             []json.RawMessage `json:"mcp_servers"`
		PermissionMode  string            `json:"permissionMode"`
		Model           string            `json:"model"`
		IsError         bool              `json:"is_error"`
		HookID          string            `json:"hook_id"`
		HookEvent       string            `json:"hook_event"`
		HookName        string            `json:"hook_name"`
		HookExit        *int              `json:"exit_code"`
		HookOutcome     string            `json:"outcome"`
		Message         *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if strictJSON(line, &w) != nil {
		fail(fixedProtocol)
		return
	}
	if w.SessionID != "" && w.SessionID != s.id || w.ParentToolUseID != nil || w.AgentID != "" {
		fail(fixedProtocol)
		return
	}
	switch w.Type {
	case "system":
		switch w.Subtype {
		case "init":
			if s.fixedInit || w.SessionID != s.id || len(w.MCP) != 0 || w.PermissionMode != "manual" || !strings.HasPrefix(w.Model, "claude-sonnet-") {
				fail(fixedInit)
				return
			}
			write := false
			for _, tool := range w.Tools {
				switch tool {
				case "Write":
					if write {
						fail(fixedInit)
						return
					}
					write = true
				case "EndConversation":
				default:
					fail(fixedInit)
					return
				}
			}
			if !write {
				fail(fixedInit)
				return
			}
			s.fixedInit = true
		case "hook_started", "hook_progress", "hook_response":
			if !s.fixedHooks.observe(w.Subtype, w.HookID, w.HookEvent, w.HookName, w.HookExit, w.HookOutcome) {
				fail(fixedHook)
				return
			}
		case "status", "api_retry":
		default:
			fail(fixedProtocol)
		}
	case "control_request":
		if !s.fixedInit || s.stopping.Load() || !s.fixedHooks.readyForWrite() {
			fail(fixedPermission)
			return
		}
		response, err := f.decide(line, s.tools)
		if err != nil {
			fail(fixedPermission)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s.enqueue(ctx, response) != nil {
			fail(fixedPermission)
		}
	case "assistant":
		if !s.fixedInit || w.Message == nil {
			fail(fixedProtocol)
			return
		}
		var blocks []block
		if strictJSON(w.Message.Content, &blocks) != nil {
			fail(fixedProtocol)
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text", "thinking", "redacted_thinking":
			case "tool_use":
				if b.Name != "Write" || !controlID.MatchString(b.ID) {
					fail(fixedPermission)
					return
				}
				if _, seen := s.tools[b.ID]; seen {
					fail(fixedPermission)
					return
				}
				s.tools[b.ID] = toolInfo{name: b.Name, input: append([]byte(nil), b.Input...)}
			default:
				fail(fixedProtocol)
				return
			}
		}
	case "user":
		// Native tool results are deliberately not published.
	case "result":
		if !s.fixedInit || w.SessionID != s.id {
			fail(fixedResult)
			return
		}
		s.mu.Lock()
		s.results++
		s.sent = 0
		s.lastOK = w.Subtype == "success" && !w.IsError && f.Approved()
		s.lastSub = "controlled"
		s.mu.Unlock()
		s.endedSelf = true
		s.closeInput()
	case "rate_limit_event", "stream_event", "control_response":
		// Bounded private metadata only. It cannot approve the write.
	default:
		fail(fixedProtocol)
	}
}
