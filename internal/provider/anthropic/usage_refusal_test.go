package anthropic

import (
	"context"
	"errors"
	"testing"
	"time"

	"fabric/internal/provider/anthropic/fakeapi"
)

// usageStart replaces the message_start event so its usage carries extra.
func usageStart(usage string) []fakeapi.Ev {
	evs := fakeapi.Text("never", 1, 1)
	evs[0].Data = `{"type":"message_start","message":{"id":"m","model":"x","usage":` + usage + `}}`
	return evs
}

func requireUsageRefused(t *testing.T, usage string) {
	t.Helper()
	s := fakeapi.New(fakeapi.Step{Events: usageStart(usage)})
	defer s.Close()
	var sleeps []time.Duration
	m, _, err := newClient(t, s, &sleeps).Send(context.Background(), body(t), Hooks{})
	var pe *ProtocolError
	if !errors.As(err, &pe) || m != nil {
		t.Fatalf("usage %s accepted: message=%+v error=%v", usage, m, err)
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("protocol error was retried: %d requests", n)
	}
}

// Usage drives billing, so a negative counter or an overflowing server-tool
// total is a protocol error, never a number to settle.
func TestNegativeTokenUsageIsProtocolError(t *testing.T) {
	for _, field := range []string{
		"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
	} {
		t.Run(field, func(t *testing.T) {
			requireUsageRefused(t, `{"`+field+`":-1}`)
		})
	}
}

func TestServerToolUsageRejectsNegativeAndOverflow(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search":-1}}`,
		`{"input_tokens":1,"output_tokens":1,"server_tool_use":{"first":9223372036854775807,"second":1}}`,
	} {
		t.Run(usage, func(t *testing.T) { requireUsageRefused(t, usage) })
	}
}

// The positive control: ordinary non-negative usage still assembles.
func TestValidServerToolUsageSums(t *testing.T) {
	s := fakeapi.New(fakeapi.Step{Events: usageStart(`{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search":2,"web_fetch":3}}`)})
	defer s.Close()
	var sleeps []time.Duration
	m, _, err := newClient(t, s, &sleeps).Send(context.Background(), body(t), Hooks{})
	if err != nil || m == nil {
		t.Fatalf("valid usage refused: %v", err)
	}
	if m.Usage.ServerTools != 5 {
		t.Fatalf("server tools %d, want 5", m.Usage.ServerTools)
	}
}
