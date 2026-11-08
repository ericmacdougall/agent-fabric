package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/provider/anthropic"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// seedLease creates one RUNNING native attempt whose lease is set by expr.
func seedLease(t *testing.T, expr string) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('m_f1', 'goodqa#15235', '{}'::jsonb, 'anthropic-api')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role, path, lock_path, lease_until)
			VALUES ('r_f1', 'm_f1', 1, 0, 0, 'RUNNING', 'h_test', 'worker', 'anthropic-api', '/nonexistent/fabric-rig/native-locks/r_f1.lock', ` + expr + `)`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return st
}

type countingTool struct{ runs int }

func (c *countingTool) Name() string         { return "probe" }
func (c *countingTool) Def() json.RawMessage { return json.RawMessage(`{"name":"probe"}`) }
func (c *countingTool) SideEffecting() bool  { return true }
func (c *countingTool) Run(context.Context, json.RawMessage) (string, bool) {
	c.runs++
	return "ran", false
}

// freshTool is the last local check before a host process starts; it is
// tested alone because the fenced DB writes around it also refuse.
func TestFreshToolRefusesStaleLease(t *testing.T) {
	for _, c := range []struct {
		name, lease string
		run         bool
	}{
		{"live", "now() + interval '1 hour'", true},
		{"expired", "now() - interval '1 second'", false},
		{"other fence", "now() + interval '1 hour'", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := seedLease(t, c.lease)
			fence := int64(1)
			if c.name == "other fence" {
				fence = 2
			}
			inner := &countingTool{}
			out, isErr := freshTool{Tool: inner, st: st, attempt: "r_f1", fence: fence}.Run(context.Background(), json.RawMessage(`{}`))
			if c.run {
				if isErr || out != "ran" || inner.runs != 1 {
					t.Fatalf("live lease: out=%q isErr=%v runs=%d", out, isErr, inner.runs)
				}
				return
			}
			if !isErr || !strings.Contains(out, "lease is stale or expired") || inner.runs != 0 {
				t.Fatalf("%s: out=%q isErr=%v runs=%d, want refusal and no run", c.name, out, isErr, inner.runs)
			}
		})
	}
}

// nativeProvider sends nothing once the lease has expired.
func TestNativeProviderRefusesExpiredLease(t *testing.T) {
	key := "sk-" + "ant-api03-" + strings.Repeat("FRESHx", 14)
	for _, c := range []struct {
		name, lease string
		sends       int
	}{
		{"live", "now() + interval '1 hour'", 1},
		{"expired", "now() - interval '1 second'", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := seedLease(t, c.lease)
			srv := fakeapi.New(fakeapi.Step{Events: fakeapi.Text("hello", 1, 1)})
			t.Cleanup(srv.Close)
			cl, err := anthropic.New(anthropic.Config{BaseURL: anthropic.ProductionBaseURL, Version: "test-version", HTTP: srv.HTTPClient(),
				Key: func(context.Context) (string, error) { return key, nil }})
			if err != nil {
				t.Fatal(err)
			}
			led := &agent.Ledger{St: st, Attempt: "r_f1", Fence: 1, Caps: []agent.Cap{{Resource: agent.SpendResource("month", "202611"), Total: 1}}}
			p := &nativeProvider{base: &anthropic.Provider{C: cl}, st: st, ledger: led, attempt: "r_f1", fence: 1}
			base := &anthropic.Provider{C: cl}
			req := agent.Request{Model: "test-model", MaxTokens: 16,
				Messages: []agent.Msg{{Role: "user", Content: []json.RawMessage{base.UserTextBlock("hi")}}}}
			_, err = p.Send(context.Background(), req, agent.Hooks{})
			if got := len(srv.Requests()); got != c.sends {
				t.Fatalf("%s lease: %d requests sent (err %v), want %d", c.name, got, err, c.sends)
			}
			if c.sends == 0 && !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("expired lease Send error = %v, want ErrInvalidTransition", err)
			}
		})
	}
}
