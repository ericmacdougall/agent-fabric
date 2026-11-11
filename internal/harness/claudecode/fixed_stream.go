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
	fail := func() { f.refuse(); _ = s.proc.Kill() }
	s.fixedBytes += len(line)
	if s.fixedBytes > 8<<20 {
		fail()
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
		Message         *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if strictJSON(line, &w) != nil {
		fail()
		return
	}
	if w.SessionID != "" && w.SessionID != s.id || w.ParentToolUseID != nil || w.AgentID != "" {
		fail()
		return
	}
	switch w.Type {
	case "system":
		switch w.Subtype {
		case "init":
			if s.fixedInit || w.SessionID != s.id || len(w.MCP) != 0 || w.PermissionMode != "manual" || !strings.HasPrefix(w.Model, "claude-sonnet-") {
				fail()
				return
			}
			write := false
			for _, tool := range w.Tools {
				switch tool {
				case "Write":
					if write {
						fail()
						return
					}
					write = true
				case "EndConversation":
				default:
					fail()
					return
				}
			}
			if !write {
				fail()
				return
			}
			s.fixedInit = true
		case "hook_started", "hook_response", "status", "api_retry":
			// Trusted native lifecycle dependency; payload remains native-private.
		default:
			fail()
		}
	case "control_request":
		if !s.fixedInit || s.stopping.Load() {
			fail()
			return
		}
		response, err := f.decide(line, s.tools)
		if err != nil {
			fail()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s.enqueue(ctx, response) != nil {
			fail()
		}
	case "assistant":
		if !s.fixedInit || w.Message == nil {
			fail()
			return
		}
		var blocks []block
		if strictJSON(w.Message.Content, &blocks) != nil {
			fail()
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text", "thinking", "redacted_thinking":
			case "tool_use":
				if b.Name != "Write" || !controlID.MatchString(b.ID) {
					fail()
					return
				}
				if _, seen := s.tools[b.ID]; seen {
					fail()
					return
				}
				s.tools[b.ID] = toolInfo{name: b.Name, input: append([]byte(nil), b.Input...)}
			default:
				fail()
				return
			}
		}
	case "user":
		// Native tool results are deliberately not published.
	case "result":
		if !s.fixedInit || w.SessionID != s.id {
			fail()
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
		fail()
	}
}
