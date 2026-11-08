package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

var canary = "sk-" + "ant-api03-" + strings.Repeat("CANARYx", 12) // the API-key shape (goodqa#15087)

func newClient(t *testing.T, s *fakeapi.Server, sleeps *[]time.Duration) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "test-version",
		Key:    func(context.Context) (string, error) { return canary, nil },
		Retry:  RetryPolicy{MaxAttempts: 4, BaseBackoff: time.Second, MaxBackoff: 8 * time.Second, MaxRetryAfter: time.Minute},
		Sleep:  func(_ context.Context, d time.Duration) error { *sleeps = append(*sleeps, d); return nil },
		Jitter: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func body(t *testing.T) []byte {
	t.Helper()
	b, err := BuildRequest(RequestSpec{Model: "m", MaxTokens: 16, Messages: []Msg{{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"hi"}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ok(text string) fakeapi.Step { return fakeapi.Step{Events: fakeapi.Text(text, 10, 5)} }

func TestSSEText(t *testing.T) {
	s := fakeapi.New(ok("hello world"))
	defer s.Close()
	var sl []time.Duration
	m, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Content) != 1 || !strings.Contains(string(m.Content[0]), `"text":"hello world"`) {
		t.Fatalf("content %s", m.Content)
	}
	if m.StopReason != "end_turn" || m.Usage.Input != 10 || m.Usage.Output != 5 {
		t.Fatalf("stop/usage %+v %s", m.Usage, m.StopReason)
	}
	if len(recs) != 1 || recs[0].RequestID == "" || recs[0].Outcome != "ok" {
		t.Fatalf("recs %+v", recs)
	}
	r := s.Requests()[0]
	if r.Header.Get("x-api-key") != canary || r.Header.Get("anthropic-version") != "test-version" {
		t.Fatal("headers not sent")
	}
}

func TestSSEToolUseAndThinking(t *testing.T) {
	in := `{"path":"README.md","note":"café <b>"}`
	s := fakeapi.New(fakeapi.Step{Events: fakeapi.ToolUse("think <hard> & well", "SIG==", "toolu_1", "read", in, 20, 30)})
	defer s.Close()
	var sl []time.Duration
	m, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Content) != 2 {
		t.Fatalf("blocks %d", len(m.Content))
	}
	var th map[string]any
	_ = json.Unmarshal(m.Content[0], &th)
	if th["thinking"] != "think <hard> & well" || th["signature"] != "SIG==" {
		t.Fatalf("thinking %v", th)
	}
	if !strings.Contains(string(m.Content[0]), "<hard> & well") {
		t.Fatal("thinking text was HTML-escaped; bytes must not drift")
	}
	tus, err := m.ToolUses()
	if err != nil || len(tus) != 1 || tus[0].ID != "toolu_1" {
		t.Fatalf("tool uses %v %v", tus, err)
	}
	var got, want any
	_ = json.Unmarshal(tus[0].Input, &got)
	_ = json.Unmarshal([]byte(in), &want)
	if !jsonEqual(got, want) {
		t.Fatalf("input %s", tus[0].Input)
	}
	if ClassifyStop(m.StopReason) != StopTools {
		t.Fatal("stop kind")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Unknown fields on a block survive assembly (AC-3 fidelity).
func TestUnknownFieldsSurvive(t *testing.T) {
	evs := []fakeapi.Ev{
		{Name: "message_start", Data: `{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":1}}}`},
		{Name: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"ENC","future_field":{"a":[1,2]}}}`},
		{Name: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Name: "content_block_start", Data: `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"","novel":"yes"}}`},
		{Name: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"a"}}`},
		{Name: "content_block_stop", Data: `{"type":"content_block_stop","index":1}`},
		{Name: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		{Name: "message_stop", Data: `{"type":"message_stop"}`},
	}
	s := fakeapi.New(fakeapi.Step{Events: evs})
	defer s.Close()
	var sl []time.Duration
	m, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m.Content[0]), `"future_field":{"a":[1,2]}`) || !strings.Contains(string(m.Content[0]), `"data":"ENC"`) {
		t.Fatalf("redacted_thinking lost fields: %s", m.Content[0])
	}
	if !strings.Contains(string(m.Content[1]), `"novel":"yes"`) {
		t.Fatalf("text block lost unknown field: %s", m.Content[1])
	}
}

func TestStopReasons(t *testing.T) {
	cases := map[string]StopKind{
		"end_turn": StopEnd, "stop_sequence": StopEnd, "tool_use": StopTools, "max_tokens": StopMaxTokens,
		"pause_turn": StopPause, "refusal": StopRefusal, "model_context_window_exceeded": StopContext,
		"brand_new_reason": StopUnknown, "": StopUnknown,
	}
	for r, want := range cases {
		if got := ClassifyStop(r); got != want {
			t.Errorf("%q: got %v want %v", r, got, want)
		}
	}
}

func TestSSEMidStreamErrorAndTruncation(t *testing.T) {
	// error event: retryable overloaded, then a good response.
	evs := fakeapi.Text("partial answer", 5, 5)
	s := fakeapi.New(
		fakeapi.Step{Events: append(append([]fakeapi.Ev{}, evs[:4]...), fakeapi.ErrorEvent("overloaded_error", "busy"))},
		fakeapi.Step{Events: evs, CutAfter: 3},                    // truncated, no message_stop
		fakeapi.Step{Events: evs, CutAfter: 3, CutMidEvent: true}, // half an event
		ok("final"),
	)
	defer s.Close()
	var sl []time.Duration
	m, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m.Content[0]), "final") {
		t.Fatalf("content %s", m.Content[0])
	}
	want := []string{"stream_error", "truncated", "truncated", "ok"}
	for i, r := range recs {
		if r.Outcome != want[i] {
			t.Fatalf("attempt %d outcome %s want %s", i+1, r.Outcome, want[i])
		}
	}
}

func TestSSEProtocolViolationsNeverComplete(t *testing.T) {
	bad := map[string][]fakeapi.Ev{
		"delta before start": {{Name: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`}},
		"unknown delta": {
			{Name: "message_start", Data: `{"type":"message_start","message":{"id":"m","usage":{}}}`},
			{Name: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{Name: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"mystery_delta","x":"y"}}`},
		},
		"stop with block open": {
			{Name: "message_start", Data: `{"type":"message_start","message":{"id":"m","usage":{}}}`},
			{Name: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{Name: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`},
			{Name: "message_stop", Data: `{"type":"message_stop"}`},
		},
		"bad tool json": {
			{Name: "message_start", Data: `{"type":"message_start","message":{"id":"m","usage":{}}}`},
			{Name: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"read","input":{}}}`},
			{Name: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`},
			{Name: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		},
		"name mismatch": {{Name: "message_stop", Data: `{"type":"message_start"}`}},
	}
	for name, evs := range bad {
		s := fakeapi.New(fakeapi.Step{Events: evs})
		var sl []time.Duration
		m, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
		s.Close()
		if err == nil || m != nil {
			t.Errorf("%s: accepted a bad stream (%v)", name, err)
		}
		if len(s.Requests()) != 1 {
			t.Errorf("%s: protocol violation was retried (%d requests)", name, len(s.Requests()))
		}
	}
}

func TestSSELimits(t *testing.T) {
	big := strings.Repeat("a", 5000)
	evs := fakeapi.Text(big, 1, 1)
	s := fakeapi.New(fakeapi.Step{Events: evs})
	defer s.Close()
	c, _ := New(Config{BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "v", Key: func(context.Context) (string, error) { return canary, nil },
		SSE: SSELimits{MaxLine: 1000, MaxEvent: 1 << 20, MaxBytes: 1 << 20, MaxEvents: 1000}})
	_, recs, err := c.Send(context.Background(), body(t), Hooks{})
	if !errors.Is(err, ErrSSELimit) {
		t.Fatalf("want ErrSSELimit, got %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("limit violation was retried: %d", len(recs))
	}
}

func TestRetryMatrix(t *testing.T) {
	t.Run("retry-after honoured on 429 529 500 504", func(t *testing.T) {
		for _, code := range []int{429, 529, 500, 504} {
			s := fakeapi.New(fakeapi.Step{Status: code, RetryAfter: "7", ErrType: "x", ErrMessage: "slow"}, ok("fine"))
			var sl []time.Duration
			_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
			s.Close()
			if err != nil || len(recs) != 2 {
				t.Fatalf("%d: err=%v recs=%d", code, err, len(recs))
			}
			if len(sl) != 1 || sl[0] != 7*time.Second {
				t.Fatalf("%d: slept %v, want 7s", code, sl)
			}
		}
	})
	t.Run("capped backoff when no retry-after", func(t *testing.T) {
		s := fakeapi.New(
			fakeapi.Step{Status: 529, ErrType: "overloaded_error", ErrMessage: "o"},
			fakeapi.Step{Status: 529, ErrType: "overloaded_error", ErrMessage: "o"},
			fakeapi.Step{Status: 500, ErrType: "api_error", ErrMessage: "o"},
			ok("fine"))
		defer s.Close()
		var sl []time.Duration
		_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
		if err != nil || len(recs) != 4 {
			t.Fatalf("err=%v recs=%d", err, len(recs))
		}
		// jitter 0 => lower half of base<<n: 0.5s, 1s, 2s (cap 8s).
		want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
		for i := range want {
			if sl[i] != want[i] {
				t.Fatalf("sleeps %v want %v", sl, want)
			}
		}
	})
	t.Run("429 without retry-after parks at once", func(t *testing.T) {
		s := fakeapi.New(fakeapi.Step{Status: 429, ErrType: "rate_limit_error", ErrMessage: "You have reached your specified API usage limits"}, ok("never"))
		defer s.Close()
		var sl []time.Duration
		_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
		if !IsPark(err) || len(recs) != 1 || len(sl) != 0 {
			t.Fatalf("err=%v recs=%d sleeps=%v", err, len(recs), sl)
		}
	})
	t.Run("400 spend limit parks at once", func(t *testing.T) {
		s := fakeapi.New(fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "Your workspace has reached its spend limit"}, ok("never"))
		defer s.Close()
		var sl []time.Duration
		_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
		if !IsPark(err) || len(recs) != 1 {
			t.Fatalf("err=%v recs=%d", err, len(recs))
		}
	})
	t.Run("plain 400 401 403 404 413 are not retried", func(t *testing.T) {
		for _, code := range []int{400, 401, 403, 404, 413} {
			s := fakeapi.New(fakeapi.Step{Status: code, ErrType: "invalid_request_error", ErrMessage: "nope"}, ok("never"))
			var sl []time.Duration
			_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
			s.Close()
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != code || len(recs) != 1 || IsPark(err) {
				t.Fatalf("%d: err=%v recs=%d", code, err, len(recs))
			}
		}
	})
	t.Run("retry-after beyond the limit parks", func(t *testing.T) {
		s := fakeapi.New(fakeapi.Step{Status: 429, RetryAfter: "3600", ErrType: "rate_limit_error", ErrMessage: "x"})
		defer s.Close()
		var sl []time.Duration
		_, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
		if !IsPark(err) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("every attempt runs BeforeAttempt and AfterAttempt", func(t *testing.T) {
		s := fakeapi.New(fakeapi.Step{Status: 529, ErrType: "overloaded_error", ErrMessage: "o"}, fakeapi.Step{Status: 500, ErrType: "api_error", ErrMessage: "o"}, ok("fine"))
		defer s.Close()
		var sl []time.Duration
		var before, after int32
		_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{
			BeforeAttempt: func(int) error { atomic.AddInt32(&before, 1); return nil },
			AfterAttempt:  func(AttemptRecord) { atomic.AddInt32(&after, 1) },
		})
		if err != nil || before != 3 || after != 3 || len(recs) != 3 {
			t.Fatalf("err=%v before=%d after=%d", err, before, after)
		}
	})
	t.Run("BeforeAttempt refusal sends nothing", func(t *testing.T) {
		s := fakeapi.New(ok("never"))
		defer s.Close()
		var sl []time.Duration
		boom := errors.New("budget")
		_, _, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{BeforeAttempt: func(int) error { return boom }})
		if !errors.Is(err, boom) || len(s.Requests()) != 0 {
			t.Fatalf("err=%v reqs=%d", err, len(s.Requests()))
		}
	})
}

func TestBuildRequestVerbatimAssistantContent(t *testing.T) {
	asst := json.RawMessage(`{"type":"thinking","thinking":"a  <b>","signature":"S","zz":  [1, 2]}`)
	b, err := BuildRequest(RequestSpec{Model: "m", MaxTokens: 5, Messages: []Msg{
		{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"q"}`)}},
		{Role: "assistant", Content: []json.RawMessage{asst}},
	}, Extra: map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"adaptive"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), string(asst)) {
		t.Fatalf("assistant block bytes were altered in the request: %s", b)
	}
	if _, err := BuildRequest(RequestSpec{Model: "m", MaxTokens: 5, Messages: []Msg{{Role: "user"}}, Extra: map[string]json.RawMessage{"stream": json.RawMessage(`false`)}}); err == nil {
		t.Fatal("reserved extra accepted")
	}
	if _, err := BuildRequest(RequestSpec{MaxTokens: 5, Messages: []Msg{{Role: "user"}}}); err == nil {
		t.Fatal("empty model accepted (the grant must name it)")
	}
}

func TestKeyNotInErrors(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{DieBeforeBody: true}, fakeapi.Step{DieBeforeBody: true}, fakeapi.Step{DieBeforeBody: true}, fakeapi.Step{DieBeforeBody: true})
	defer s.Close()
	var sl []time.Duration
	_, recs, err := newClient(t, s, &sl).Send(context.Background(), body(t), Hooks{})
	if err == nil {
		t.Fatal("expected error")
	}
	all := err.Error()
	for _, r := range recs {
		all += r.Err
	}
	if strings.Contains(all, canary) {
		t.Fatal("key leaked into error text")
	}
}

func TestKeyFromCommand(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/good.sh"
	if err := os.WriteFile(good, []byte("#!/bin/sh\nprintf '%s\\n' '"+canary+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := dir + "/bad.sh"
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho '"+canary+"'\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	multi := dir + "/multi.sh"
	if err := os.WriteFile(multi, []byte("#!/bin/sh\necho 'two words'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ks, err := KeyFromCommand([]string{good}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := ks(context.Background()); err != nil || k != canary {
		t.Fatalf("key %q err %v", k, err)
	}
	kb, _ := KeyFromCommand([]string{bad}, time.Minute, nil)
	if _, err := kb(context.Background()); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("failing command: err=%v (must fail and not echo the output)", err)
	}
	km, _ := KeyFromCommand([]string{multi}, time.Minute, nil)
	if _, err := km(context.Background()); err == nil {
		t.Fatal("multi-token output accepted as a key")
	}
	if _, err := KeyFromCommand(nil, time.Minute, nil); err == nil {
		t.Fatal("empty command accepted")
	}
}

func TestCaptureNeverSeesKey(t *testing.T) {
	s := fakeapi.New(ok("captured"))
	defer s.Close()
	var got []Capture
	c, _ := New(Config{BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "v", Key: func(context.Context) (string, error) { return canary, nil },
		Capture: func(cp Capture) { got = append(got, cp) }})
	if _, _, err := c.Send(context.Background(), body(t), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != 200 || got[0].RequestID == "" || !strings.Contains(string(got[0].ResponseBody), "message_stop") {
		t.Fatalf("capture %+v", got)
	}
	all := fmt.Sprint(got[0].Header) + string(got[0].RequestBody) + string(got[0].ResponseBody)
	if strings.Contains(all, canary) || strings.Contains(strings.ToLower(all), "x-api-key") {
		t.Fatal("capture contains credentials")
	}
}
