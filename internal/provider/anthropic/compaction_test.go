package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic/fakeapi"
)

func clientWith(t *testing.T, s *fakeapi.Server, betas ...string) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL: ProductionBaseURL, HTTP: s.HTTPClient(), Version: "test-version", Betas: betas,
		Key:    func(context.Context) (string, error) { return canary, nil },
		Retry:  RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Second, MaxBackoff: time.Second, MaxRetryAfter: time.Minute},
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Jitter: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBetaHeader(t *testing.T) {
	s := fakeapi.New(ok("a"), ok("b"))
	defer s.Close()
	if _, _, err := clientWith(t, s).Send(context.Background(), body(t), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if h := s.Requests()[0].Header.Get("anthropic-beta"); h != "" {
		t.Fatalf("a beta header was sent with none configured: %q", h)
	}
	if _, _, err := clientWith(t, s, "compact-2026-01-12").Send(context.Background(), body(t), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if h := s.Requests()[1].Header.Get("anthropic-beta"); h != "compact-2026-01-12" {
		t.Fatalf("beta header %q", h)
	}
	if _, err := New(Config{BaseURL: ProductionBaseURL, Version: "v", Key: func(context.Context) (string, error) { return "", nil }, Betas: []string{"context-1m-2025-08-07"}}); err == nil {
		t.Fatal("a beta outside the allowlist was accepted")
	}
}

func TestCompactionBlockReplay(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{Events: fakeapi.Compacted("SUMMARY of work so far", "continuing", 120, 30, 9000, 700)}, ok("next"))
	defer s.Close()
	p := &Provider{C: clientWith(t, s, "compact-2026-01-12")}
	res, err := p.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 100,
		Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{p.UserTextBlock("hi")}}}}, agent.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 2 || !strings.Contains(string(res.Content[0]), `"type":"compaction"`) || !strings.Contains(string(res.Content[0]), "SUMMARY of work so far") {
		t.Fatalf("compaction block not assembled: %s", res.Content)
	}
	// stored verbatim and replayed byte-for-byte
	_, err = p.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 100, Messages: []agent.Msg{
		{Role: "assistant", Content: res.Content},
		{Role: "user", Content: []json.RawMessage{p.UserTextBlock("go on")}}}}, agent.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(s.Requests()[1].Body), string(res.Content[0])) {
		t.Fatalf("the compaction block was not replayed verbatim: %s", s.Requests()[1].Body)
	}
	// a null summary stays null
	s2 := fakeapi.New(fakeapi.Step{Events: fakeapi.Compacted("", "x", 1, 1, 1, 1)})
	defer s2.Close()
	p2 := &Provider{C: clientWith(t, s2, "compact-2026-01-12")}
	res2, err := p2.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 10,
		Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{p.UserTextBlock("hi")}}}}, agent.Hooks{})
	if err != nil || !strings.Contains(string(res2.Content[0]), `"content":null`) {
		t.Fatalf("null summary: %v %s", err, res2.Content)
	}
}

const testPrices = `{"input_per_mtok_micros":100000,"output_per_mtok_micros":500000,"cache_write_per_mtok_micros":200000,"cache_read_per_mtok_micros":10000,"server_tool_per_use_micros":10000,"long_context_threshold_tokens":0,"long_context_multiplier_percent":100}`

func TestChargeIterations(t *testing.T) {
	pr, err := agent.ParsePrices(json.RawMessage(testPrices))
	if err != nil {
		t.Fatal(err)
	}
	s := fakeapi.New(fakeapi.Step{Events: fakeapi.Compacted("S", "t", 1000, 100, 50000, 2000)})
	defer s.Close()
	p := &Provider{C: clientWith(t, s, "compact-2026-01-12")}
	res, err := p.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 100,
		Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{p.UserTextBlock("hi")}}}}, agent.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage.Iterations) != 2 || res.Usage.Iterations[0].Type != "compaction" {
		t.Fatalf("iterations not parsed: %+v", res.Usage)
	}
	withComp := pr.Cost(res.Usage)
	top := res.Usage
	top.Iterations = nil
	if withComp <= pr.Cost(top) {
		t.Fatalf("the compaction pass was not charged: %d vs top-level %d", withComp, pr.Cost(top))
	}
	// 51000 input tokens at 100000 micros per Mtok = 5100, 2100 output at 500000 = 1050
	if want := int64(5100 + 1050); withComp != want {
		t.Fatalf("cost %d want %d", withComp, want)
	}
}

func TestCompactionBetaRejected(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "unsupported beta header: compact-2026-01-12"})
	defer s.Close()
	p := &Provider{C: clientWith(t, s, "compact-2026-01-12")}
	_, err := p.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 10,
		Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{p.UserTextBlock("hi")}}}}, agent.Hooks{})
	var pe *agent.ParkError
	if !errors.As(err, &pe) || !strings.Contains(pe.Reason, "unsupported beta header: compact-2026-01-12") {
		t.Fatalf("want a park with the API's words, got %v", err)
	}
}

func TestPromptTooLongIsContextOverflow(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "prompt is too long: 250000 tokens > 200000 maximum"})
	defer s.Close()
	p := &Provider{C: clientWith(t, s)}
	_, err := p.Send(context.Background(), agent.Request{Model: "m", MaxTokens: 10,
		Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{p.UserTextBlock("hi")}}}}, agent.Hooks{})
	if !errors.Is(err, agent.ErrContextOverflow) {
		t.Fatalf("got %v", err)
	}
}
