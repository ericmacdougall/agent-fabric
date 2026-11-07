// Package fakeapi is a scriptable Anthropic Messages server for tests: it
// scripts statuses and streams, truncates, stalls, and validates replayed
// history the way the real API does (thinking blocks unchanged, tool_result
// pairing). It lives outside _test files so internal/agent tests use it too.
package fakeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Ev is one SSE event.
type Ev struct{ Name, Data string }

// Step scripts one HTTP response. Exactly one request consumes one step.
type Step struct {
	Status     int // 0 = 200
	RetryAfter string
	ErrType    string
	ErrMessage string
	Events     []Ev
	// CutAfter truncates the stream (connection closed without message_stop)
	// after this many events when > 0; CutMidEvent also writes half an event.
	CutAfter    int
	CutMidEvent bool
	// Stall sleeps before each event (slow-loris) when > 0.
	Stall time.Duration
	// DieBeforeBody makes the server hang up with no response at all.
	DieBeforeBody bool
}

// Recorded is one request as the server saw it.
type Recorded struct {
	Body   []byte
	Header http.Header
	SHA    string
}

// Server is the fake.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	steps []Step
	Reqs  []Recorded
	// Validate, when set, may reject a request with a 400 message.
	Validate func(body []byte) error
	// BindThinking enables prefix-binding checks: a thinking block resent in
	// the latest assistant message must equal what this server emitted.
	emitted map[string]string // signature -> canonical thinking block
	hold    chan struct{}
}

func New(steps ...Step) *Server {
	s := &Server{steps: steps, emitted: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Push appends steps.
func (s *Server) Push(st ...Step) { s.mu.Lock(); s.steps = append(s.steps, st...); s.mu.Unlock() }

// Requests returns a copy of what was received.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.Reqs...)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	s.mu.Lock()
	s.Reqs = append(s.Reqs, Recorded{Body: body, Header: r.Header.Clone(), SHA: hex.EncodeToString(sum[:])})
	var st Step
	if len(s.steps) == 0 {
		st = Step{Status: 500, ErrType: "api_error", ErrMessage: "fakeapi: script exhausted"}
	} else {
		st, s.steps = s.steps[0], s.steps[1:]
	}
	validate := s.Validate
	s.mu.Unlock()
	w.Header().Set("request-id", fmt.Sprintf("req_fake%04d", len(s.Reqs)))
	if st.DieBeforeBody {
		if hj, ok := w.(http.Hijacker); ok {
			c, _, _ := hj.Hijack()
			c.Close()
		}
		return
	}
	if err := checkHistory(body, s); err != nil && st.Status == 0 {
		writeErr(w, 400, "invalid_request_error", err.Error(), "")
		return
	}
	if validate != nil {
		if err := validate(body); err != nil && st.Status == 0 {
			writeErr(w, 400, "invalid_request_error", err.Error(), "")
			return
		}
	}
	if st.Status != 0 && st.Status/100 != 2 {
		writeErr(w, st.Status, st.ErrType, st.ErrMessage, st.RetryAfter)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	blocks := map[int]map[string]any{}
	for i, e := range st.Events {
		if st.CutAfter > 0 && i == st.CutAfter {
			if st.CutMidEvent {
				fmt.Fprintf(w, "event: %s\ndata: {\"type\":", e.Name)
				fl.Flush()
			}
			return
		}
		if st.Stall > 0 {
			time.Sleep(st.Stall)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, e.Data)
		if fl != nil {
			fl.Flush()
		}
		s.track(blocks, e)
	}
}

// track rebuilds thinking blocks from the events it sends, so a replayed
// block can be compared whole (text, signature and any unknown field).
func (s *Server) track(blocks map[int]map[string]any, e Ev) {
	var h struct {
		Type  string         `json:"type"`
		Index int            `json:"index"`
		Block map[string]any `json:"content_block"`
		Delta map[string]any `json:"delta"`
	}
	if json.Unmarshal([]byte(e.Data), &h) != nil {
		return
	}
	switch h.Type {
	case "content_block_start":
		if h.Block["type"] == "thinking" {
			blocks[h.Index] = h.Block
		}
	case "content_block_delta":
		b := blocks[h.Index]
		if b == nil {
			return
		}
		switch h.Delta["type"] {
		case "thinking_delta":
			cur, _ := b["thinking"].(string)
			add, _ := h.Delta["thinking"].(string)
			b["thinking"] = cur + add
		case "signature_delta":
			b["signature"] = h.Delta["signature"]
		}
	case "content_block_stop":
		if b := blocks[h.Index]; b != nil {
			sig, _ := b["signature"].(string)
			canon, _ := json.Marshal(b)
			s.mu.Lock()
			s.emitted[sig] = string(canon)
			s.mu.Unlock()
		}
	}
}

func writeErr(w http.ResponseWriter, status int, typ, msg, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("retry-after", retryAfter)
	}
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q},"request_id":"req_fake"}`, typ, msg)
}

// checkHistory enforces the two rules that bite a replaying client: every
// tool_use has a following tool_result, and thinking blocks carry a
// signature this server issued (an edited block fails the signature).
func checkHistory(body []byte, s *Server) error {
	var req struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("body is not JSON: %v", err)
	}
	for i, m := range req.Messages {
		var uses []string
		for _, c := range m.Content {
			var b struct {
				Type, ID, Signature, Thinking string
			}
			_ = json.Unmarshal(c, &b)
			var whole map[string]any
			_ = json.Unmarshal(c, &whole)
			canon, _ := json.Marshal(whole)
			if b.Type == "tool_use" {
				uses = append(uses, b.ID)
			}
			if b.Type == "thinking" && m.Role == "assistant" {
				s.mu.Lock()
				want, known := s.emitted[b.Signature]
				s.mu.Unlock()
				if !known || want != string(canon) {
					return fmt.Errorf("messages.%d: `thinking` blocks in the latest assistant message cannot be modified", i)
				}
			}
		}
		if len(uses) > 0 {
			if i+1 >= len(req.Messages) || req.Messages[i+1].Role != "user" {
				return fmt.Errorf("messages.%d: tool_use ids were found without tool_result blocks immediately after", i)
			}
			have := map[string]bool{}
			for _, c := range req.Messages[i+1].Content {
				var b struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				}
				_ = json.Unmarshal(c, &b)
				if b.Type == "tool_result" {
					have[b.ToolUseID] = true
				}
			}
			for _, id := range uses {
				if !have[id] {
					return fmt.Errorf("messages.%d: tool_use id %s has no tool_result", i, id)
				}
			}
		}
	}
	return nil
}

// ---- stream builders ----

func ev(name, data string) Ev { return Ev{Name: name, Data: data} }

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func start(in, out int) Ev {
	return ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","model":"fake-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}}`, in, out))
}

func end(reason string, out int) []Ev {
	return []Ev{
		ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, reason, out)),
		ev("message_stop", `{"type":"message_stop"}`),
	}
}

// Text streams one text block split in two deltas.
func Text(text string, in, out int) []Ev {
	h := len(text) / 2
	e := []Ev{start(in, 1),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("ping", `{"type":"ping"}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+js(text[:h])+`}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+js(text[h:])+`}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	}
	return append(e, end("end_turn", out)...)
}

// ToolUse streams optional thinking, a text block, then a tool_use whose
// input arrives in three partial_json fragments.
func ToolUse(think, sig, id, name, inputJSON string, in, out int) []Ev {
	e := []Ev{start(in, 1)}
	idx := 0
	if think != "" {
		e = append(e,
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"","provider_meta":{"k":[1,2]}}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":`+js(think)+`}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":`+js(sig)+`}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`))
		idx = 1
	}
	third := len(inputJSON) / 3
	e = append(e,
		ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, idx, id, name)),
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, idx)),
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, idx, js(inputJSON[:third]))),
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, idx, js(inputJSON[third:2*third]))),
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, idx, js(inputJSON[2*third:]))),
		ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx)))
	return append(e, end("tool_use", out)...)
}

// StopWith streams a text block that stops for the given reason.
func StopWith(reason string, in, out int) []Ev {
	e := Text("x", in, out)
	e = e[:len(e)-2]
	return append(e, end(reason, out)...)
}

// WithCache returns a stream whose usage carries cache counters and thinking
// output so budget tests see every price field exercised.
func WithCache(text string, in, out, cacheCreate, cacheRead int) []Ev {
	e := Text(text, in, out)
	e[0] = ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","model":"fake-model","content":[],"stop_reason":null,"usage":{"input_tokens":%d,"output_tokens":1,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`, in, cacheCreate, cacheRead))
	return e
}

// ErrorEvent is a mid-stream error event.
func ErrorEvent(typ, msg string) Ev {
	return ev("error", fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg))
}
