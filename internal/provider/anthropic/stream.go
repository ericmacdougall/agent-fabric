package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Usage is the cumulative token usage of one response. Fields the API did
// not send stay zero; Raw is the last usage object verbatim for the audit
// record (it can carry server-tool counters this code does not act on).
type Usage struct {
	Input       int64           `json:"input_tokens"`
	Output      int64           `json:"output_tokens"`
	CacheCreate int64           `json:"cache_creation_input_tokens"`
	CacheRead   int64           `json:"cache_read_input_tokens"`
	ServerTools int64           `json:"server_tool_uses"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

// Message is a fully assembled assistant message. Content blocks are kept as
// raw JSON: the loop persists and resends exactly these bytes, so thinking
// text, signatures and fields this code has never heard of survive intact.
type Message struct {
	ID           string
	Model        string
	Content      []json.RawMessage
	StopReason   string
	StopSequence string
	Usage        Usage
	RequestID    string
}

// ProtocolError is a stream the assembler refuses to interpret. Nothing in a
// response that ends in one is executed.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "anthropic protocol: " + e.Msg }

// StreamError is an in-stream `error` event (HTTP status was already 200).
type StreamError struct {
	Type, Message string
}

func (e *StreamError) Error() string { return "anthropic stream error: " + e.Type + ": " + e.Message }

// Retryable reports whether the same request may be sent again.
func (e *StreamError) Retryable() bool {
	return e.Type == "overloaded_error" || e.Type == "api_error" || e.Type == "timeout_error"
}

type blockState struct {
	typ     string
	fields  map[string]json.RawMessage
	text    strings.Builder
	think   strings.Builder
	sig     strings.Builder
	hasSig  bool
	partial bytes.Buffer
	cites   []json.RawMessage
	stopped bool
}

type assembler struct {
	msg     Message
	blocks  []*blockState
	started bool
	done    bool
	// stoppedAny is true once any content_block_stop arrived. Assembly never
	// exposes a block before its stop, and the loop never acts before done.
}

func perr(f string, a ...any) error { return &ProtocolError{Msg: fmt.Sprintf(f, a...)} }

type usageWire struct {
	Input       *int64           `json:"input_tokens"`
	Output      *int64           `json:"output_tokens"`
	CacheCreate *int64           `json:"cache_creation_input_tokens"`
	CacheRead   *int64           `json:"cache_read_input_tokens"`
	ServerTool  map[string]int64 `json:"server_tool_use"`
}

func (a *assembler) mergeUsage(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var w usageWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return perr("usage: %v", err)
	}
	// Usage is billed, so a negative counter or a server-tool total that would
	// overflow is refused as a protocol error rather than settled.
	for _, c := range []struct {
		name string
		dst  *int64
		src  *int64
	}{
		{"input_tokens", &a.msg.Usage.Input, w.Input},
		{"output_tokens", &a.msg.Usage.Output, w.Output},
		{"cache_creation_input_tokens", &a.msg.Usage.CacheCreate, w.CacheCreate},
		{"cache_read_input_tokens", &a.msg.Usage.CacheRead, w.CacheRead},
	} {
		if c.src == nil {
			continue
		}
		if *c.src < 0 {
			return perr("usage %s is negative", c.name)
		}
		*c.dst = *c.src
	}
	if w.ServerTool != nil {
		var total int64
		for name, n := range w.ServerTool {
			if n < 0 {
				return perr("usage server_tool_use.%s is negative", name)
			}
			if n > math.MaxInt64-total {
				return perr("usage server_tool_use total overflows int64")
			}
			total += n
		}
		a.msg.Usage.ServerTools = total
	}
	a.msg.Usage.Raw = bytes.Clone(raw)
	return nil
}

// feed applies one event. It returns (true, nil) after message_stop.
func (a *assembler) feed(ev sseEvent) (bool, error) {
	if a.done {
		return false, perr("event after message_stop")
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(ev.Data, &head); err != nil {
		return false, perr("event data is not JSON (%s)", ev.Name)
	}
	if ev.Name != "" && ev.Name != head.Type {
		return false, perr("event name %q does not match type %q", ev.Name, head.Type)
	}
	if head.Type != "message_start" && head.Type != "error" && head.Type != "ping" && !a.started {
		return false, perr("%s before message_start", head.Type)
	}
	switch head.Type {
	case "ping":
		return false, nil
	case "error":
		var e struct {
			Error struct{ Type, Message string } `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		return false, &StreamError{Type: e.Error.Type, Message: e.Error.Message}
	case "message_start":
		if a.started {
			return false, perr("second message_start")
		}
		var m struct {
			Message struct {
				ID    string          `json:"id"`
				Model string          `json:"model"`
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return false, perr("message_start: %v", err)
		}
		a.started = true
		a.msg.ID, a.msg.Model = m.Message.ID, m.Message.Model
		return false, a.mergeUsage(m.Message.Usage)
	case "content_block_start":
		var s struct {
			Index int             `json:"index"`
			Block json.RawMessage `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &s); err != nil {
			return false, perr("content_block_start: %v", err)
		}
		if s.Index != len(a.blocks) {
			return false, perr("content_block_start index %d, expected %d", s.Index, len(a.blocks))
		}
		if n := len(a.blocks); n > 0 && !a.blocks[n-1].stopped {
			return false, perr("block %d started before block %d stopped", s.Index, n-1)
		}
		b := &blockState{fields: map[string]json.RawMessage{}}
		if err := json.Unmarshal(s.Block, &b.fields); err != nil {
			return false, perr("content_block_start block: %v", err)
		}
		if err := json.Unmarshal(b.fields["type"], &b.typ); err != nil || b.typ == "" {
			return false, perr("block without type")
		}
		if b.typ == "text" || b.typ == "thinking" {
			// Seed with what the start event carried, deltas append.
			var t string
			key := "text"
			dst := &b.text
			if b.typ == "thinking" {
				key, dst = "thinking", &b.think
			}
			if v, ok := b.fields[key]; ok {
				if err := json.Unmarshal(v, &t); err != nil {
					return false, perr("%s start field: %v", key, err)
				}
				dst.WriteString(t)
			}
			if v, ok := b.fields["signature"]; ok && b.typ == "thinking" {
				var sg string
				if err := json.Unmarshal(v, &sg); err == nil {
					b.sig.WriteString(sg)
					b.hasSig = true
				}
			}
		}
		a.blocks = append(a.blocks, b)
		return false, nil
	case "content_block_delta":
		var d struct {
			Index int             `json:"index"`
			Delta json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return false, perr("content_block_delta: %v", err)
		}
		if d.Index < 0 || d.Index >= len(a.blocks) || a.blocks[d.Index].stopped {
			return false, perr("delta for block %d which is not open", d.Index)
		}
		return false, a.blocks[d.Index].delta(d.Delta)
	case "content_block_stop":
		var s struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(ev.Data, &s); err != nil {
			return false, perr("content_block_stop: %v", err)
		}
		if s.Index < 0 || s.Index >= len(a.blocks) || a.blocks[s.Index].stopped {
			return false, perr("stop for block %d which is not open", s.Index)
		}
		return false, a.blocks[s.Index].finish()
	case "message_delta":
		var d struct {
			Delta struct {
				StopReason   *string `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
			} `json:"delta"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return false, perr("message_delta: %v", err)
		}
		if d.Delta.StopReason != nil {
			a.msg.StopReason = *d.Delta.StopReason
		}
		if d.Delta.StopSequence != nil {
			a.msg.StopSequence = *d.Delta.StopSequence
		}
		return false, a.mergeUsage(d.Usage)
	case "message_stop":
		for i, b := range a.blocks {
			if !b.stopped {
				return false, perr("message_stop with block %d open", i)
			}
		}
		if a.msg.StopReason == "" {
			return false, perr("message_stop without stop_reason")
		}
		a.done = true
		for _, b := range a.blocks {
			raw, err := b.marshal()
			if err != nil {
				return false, err
			}
			a.msg.Content = append(a.msg.Content, raw)
		}
		return true, nil
	default:
		// Unknown event types may be added by the API; they carry nothing the
		// loop acts on, so they are skipped (versioning policy).
		return false, nil
	}
}

func (b *blockState) delta(raw json.RawMessage) error {
	var d map[string]json.RawMessage
	if err := json.Unmarshal(raw, &d); err != nil {
		return perr("delta: %v", err)
	}
	var typ string
	_ = json.Unmarshal(d["type"], &typ)
	str := func(k string) (string, error) {
		var s string
		if err := json.Unmarshal(d[k], &s); err != nil {
			return "", perr("%s delta field %q: %v", typ, k, err)
		}
		return s, nil
	}
	switch typ {
	case "text_delta":
		s, err := str("text")
		b.text.WriteString(s)
		return err
	case "thinking_delta":
		s, err := str("thinking")
		b.think.WriteString(s)
		return err
	case "signature_delta":
		s, err := str("signature")
		b.sig.WriteString(s)
		b.hasSig = true
		return err
	case "input_json_delta":
		s, err := str("partial_json")
		if b.partial.Len()+len(s) > 8<<20 {
			return perr("tool input exceeds 8 MiB")
		}
		b.partial.WriteString(s)
		return err
	case "citations_delta":
		b.cites = append(b.cites, d["citation"])
		return nil
	default:
		// Dropping a delta we do not understand would silently change the
		// message the model produced; refuse the response instead.
		return perr("unknown delta type %q", typ)
	}
}

func enc(s string) json.RawMessage {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (b *blockState) finish() error {
	b.stopped = true
	switch b.typ {
	case "text":
		b.fields["text"] = enc(b.text.String())
		if len(b.cites) > 0 {
			var existing []json.RawMessage
			if v, ok := b.fields["citations"]; ok {
				_ = json.Unmarshal(v, &existing)
			}
			all, _ := json.Marshal(append(existing, b.cites...))
			b.fields["citations"] = all
		}
	case "thinking":
		b.fields["thinking"] = enc(b.think.String())
		if b.hasSig {
			b.fields["signature"] = enc(b.sig.String())
		}
	case "tool_use", "server_tool_use", "mcp_tool_use":
		in := bytes.TrimSpace(b.partial.Bytes())
		if len(in) == 0 {
			// No deltas: the start event's input (normally {}) stands.
			if _, ok := b.fields["input"]; !ok {
				in = []byte("{}")
			}
		}
		if len(in) > 0 {
			if !json.Valid(in) || in[0] != '{' {
				return perr("tool input is not a JSON object")
			}
			b.fields["input"] = bytes.Clone(in)
		}
	}
	return nil
}

func (b *blockState) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(b.fields); err != nil {
		return nil, perr("marshal block: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ToolUse is the part of a tool_use block the loop acts on.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolUses extracts client tool calls from assembled content. A tool_use
// without an id or name is a protocol error, not a call.
func (m *Message) ToolUses() ([]ToolUse, error) {
	var out []ToolUse
	for _, c := range m.Content {
		var h struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c, &h) != nil {
			return nil, perr("unreadable content block")
		}
		if h.Type != "tool_use" {
			continue
		}
		var tu ToolUse
		if err := json.Unmarshal(c, &tu); err != nil || tu.ID == "" || tu.Name == "" {
			return nil, perr("malformed tool_use block")
		}
		out = append(out, tu)
	}
	return out, nil
}

// StopKind classifies a stop_reason. Unknown reasons are never treated as a
// normal end of turn.
type StopKind int

const (
	StopUnknown StopKind = iota
	StopEnd
	StopTools
	StopMaxTokens
	StopPause
	StopRefusal
	StopContext
)

func ClassifyStop(reason string) StopKind {
	switch reason {
	case "end_turn", "stop_sequence":
		return StopEnd
	case "tool_use":
		return StopTools
	case "max_tokens":
		return StopMaxTokens
	case "pause_turn":
		return StopPause
	case "refusal":
		return StopRefusal
	case "model_context_window_exceeded":
		return StopContext
	}
	return StopUnknown
}

// ErrPark marks a condition that parks the mission with a human reason
// instead of retrying.
type ParkError struct{ Reason string }

func (e *ParkError) Error() string { return "park: " + e.Reason }

func IsPark(err error) bool { var p *ParkError; return errors.As(err, &p) }
