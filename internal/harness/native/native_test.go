package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
	"fabric/internal/harness/native"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "hnative")) }

const (
	attemptID = "r_hn1"
	missionID = "m_hn1"
)

// canary is built at run time so no key-shaped literal sits in the source.
var canary = "sk-" + "ant-api03-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func i64(v int64) *int64 { return &v }

func prices() agent.Prices {
	return agent.Prices{Input: i64(3_000_000), Output: i64(15_000_000), CacheWrite: i64(3_750_000), CacheRead: i64(300_000),
		ServerToolUse: i64(10_000), LongContextThreshold: i64(200_000), LongContextPercent: i64(200)}
}

// stubProvider is a scripted agent.Provider that honours the loop's hooks.
type stubProvider struct {
	mu    sync.Mutex
	calls int
	step  func(call int, ctx context.Context) (*agent.Result, error)
}

func (p *stubProvider) Name() string     { return "stub" }
func (p *stubProvider) Caps() agent.Caps { return agent.Caps{ResultsInUserMessage: true} }
func (p *stubProvider) UserTextBlock(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
	return b
}
func (p *stubProvider) ToolResultBlock(id, text string, isErr bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr})
	return b
}
func (p *stubProvider) Send(ctx context.Context, req agent.Request, h agent.Hooks) (*agent.Result, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if h.BeforeAttempt != nil {
		if err := h.BeforeAttempt(1, 100); err != nil {
			return nil, err
		}
	}
	res, err := p.step(n, ctx)
	if err != nil {
		return nil, err
	}
	if h.AfterAttempt != nil {
		h.AfterAttempt(agent.AttemptRecord{N: 1, Status: 200, Outcome: "ok", Usage: &res.Usage})
	}
	return res, nil
}

func blocks(bs ...map[string]any) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range bs {
		x, _ := json.Marshal(b)
		out = append(out, x)
	}
	return out
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func endResult(bs ...map[string]any) *agent.Result {
	return &agent.Result{Content: blocks(bs...), StopReason: "end_turn", Stop: agent.StopEnd, Usage: agent.Usage{Input: 10, Output: 5}, Model: "test-model", RequestSHA: "x"}
}

type echoTool struct {
	agent.Tool
	runs *int
}

func (e *echoTool) Name() string        { return "echo" }
func (e *echoTool) SideEffecting() bool { return false }
func (e *echoTool) Def() json.RawMessage {
	return json.RawMessage(`{"name":"echo","description":"t","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}`)
}
func (e *echoTool) Run(_ context.Context, in json.RawMessage) (string, bool) {
	*e.runs++
	return "ran", false
}

type loopEnv struct {
	prov *stubProvider
	runs *int
}

// adapter builds a native adapter over a real loop and database.
func adapter(t harnesstest.T, tt *testing.T, step func(int, context.Context) (*agent.Result, error)) (*native.Adapter, *loopEnv) {
	tt.Helper()
	st := storetest.Open(tt)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO missions (id, ticket, state, provider) VALUES ('` + missionID + `', 'goodqa#15373', '{}'::jsonb, 'anthropic-api')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role, path, lock_path, lease_until)
			VALUES ('` + attemptID + `', '` + missionID + `', 1, 0, 0, 'RUNNING', 'h_test', 'worker', 'anthropic-api', '/nonexistent/fabric-rig/native-locks/` + attemptID + `.lock', now() + interval '1 hour')`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			tt.Fatalf("seed: %v", err)
		}
	}
	env := &loopEnv{prov: &stubProvider{step: step}, runs: new(int)}
	a := &native.Adapter{Build: func(spec harness.StartSpec, o *native.Observer) (*agent.Loop, error) {
		led := &agent.Ledger{St: st, Attempt: attemptID, Fence: 1, TTL: time.Hour, Caps: []agent.Cap{
			{Resource: agent.SpendResource("month", "202611"), Total: 100_000_000},
			{Resource: agent.SpendResource("job", missionID), Total: 5_000_000},
			{Resource: agent.SpendResource("run", attemptID), Total: 1_000_000},
		}}
		return agent.New(agent.Config{
			DB: &agent.DB{Pool: st.Pool()}, Ledger: led, Provider: o.Provider(env.prov),
			Tools:  o.Tools([]agent.Tool{&echoTool{runs: env.runs}}),
			Redact: harness.PatternRedactor(), Attempt: attemptID, Fence: 1, Model: "test-model",
			MaxTokens: 1000, System: "sys", Prompt: "do the task", MaxTurns: 10, Wall: time.Hour,
			RestartCap: 20, Prices: prices(), BillingMode: agent.BillingAPIKey,
		})
	}}
	return a, env
}

func TestNativeConformance(t *testing.T) {
	normal := func(call int, _ context.Context) (*agent.Result, error) {
		if call == 1 {
			r := &agent.Result{
				Content:    blocks(text("starting"), map[string]any{"type": "tool_use", "id": "toolu_1", "name": "echo", "input": map[string]string{"cmd": "a"}}),
				StopReason: "tool_use", Stop: agent.StopTools, Usage: agent.Usage{Input: 10, Output: 5}, Model: "test-model", RequestSHA: "x",
				ToolUses: []agent.ToolUse{{ID: "toolu_1", Name: "echo", Input: json.RawMessage(`{"cmd":"a"}`)}},
			}
			return r, nil
		}
		return endResult(text("all done")), nil
	}
	harnesstest.Conformance(t, harnesstest.Case{
		Normal: func(h harnesstest.T) harness.Adapter { a, _ := adapter(h, t, normal); return a },
		Unknown: func(h harnesstest.T) harness.Adapter {
			a, _ := adapter(h, t, func(int, context.Context) (*agent.Result, error) {
				return endResult(text("hi"), map[string]any{"type": "future_block", "x": 1}), nil
			})
			return a
		},
		NoFinal: func(h harnesstest.T) harness.Adapter {
			a, _ := adapter(h, t, func(call int, _ context.Context) (*agent.Result, error) {
				if call == 1 {
					return endResult(text("hi")), errors.New("transport exploded")
				}
				return nil, errors.New("transport exploded")
			})
			return a
		},
		Secret: func(h harnesstest.T) (harness.Adapter, string) {
			a, _ := adapter(h, t, func(int, context.Context) (*agent.Result, error) {
				return endResult(text("found " + canary + " here")), nil
			})
			return a, canary
		},
		Block: func(h harnesstest.T) harness.Adapter {
			a, _ := adapter(h, t, func(_ int, ctx context.Context) (*agent.Result, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			return a
		},
	})
}

func TestNativeCaps(t *testing.T) {
	a := &native.Adapter{}
	if c := a.Caps(); c.Interactive || !c.Resume {
		t.Fatalf("caps %+v", c)
	}
	if a.Name() != "native" {
		t.Fatal(a.Name())
	}
	r := harness.NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
}

// reviewTool is a DurableReviewTool; the observer must keep it one.
type reviewTool struct{ echoTool }

func (r *reviewTool) RunReviewed(ctx context.Context, in json.RawMessage) (agent.ReviewRunResult, error) {
	return agent.ReviewRunResult{GuardDenied: true, IsError: true, Output: "denied"}, nil
}

func TestObserverKeepsDurableReviewInterface(t *testing.T) {
	rec := &harnesstest.Recorder{}
	runs := 0
	a := &native.Adapter{Build: func(spec harness.StartSpec, ob *native.Observer) (*agent.Loop, error) {
		wrapped := ob.Tools([]agent.Tool{&reviewTool{echoTool{runs: &runs}}, &echoTool{runs: &runs}})
		if _, ok := wrapped[0].(agent.DurableReviewTool); !ok {
			t.Error("wrapping hid DurableReviewTool: the review gate would be skipped")
		}
		if _, ok := wrapped[1].(agent.DurableReviewTool); ok {
			t.Error("a plain tool became a DurableReviewTool")
		}
		res, err := wrapped[0].(agent.DurableReviewTool).RunReviewed(context.Background(), json.RawMessage(`{"cmd":"rm"}`))
		if err != nil || !res.GuardDenied {
			t.Errorf("result changed by the observer: %+v %v", res, err)
		}
		return nil, errors.New("stop here")
	}}
	if _, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_x"}, rec); err == nil {
		t.Fatal("builder error swallowed")
	}
	found := false
	for _, e := range rec.Snapshot() {
		if e.Kind == harness.KindToolCall && string(e.Payload) != "" {
			found = true
			var tc harness.ToolCall
			_ = json.Unmarshal(e.Payload, &tc)
			if tc.Status != "denied" {
				t.Errorf("status %q, want denied", tc.Status)
			}
		}
	}
	if !found {
		t.Fatal("no tool_call event for the reviewed tool")
	}
}
