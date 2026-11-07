package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/provider/anthropic"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "agent")) }

// canary is built at run time so no key-shaped literal sits in the source.
var canary = "sk-" + "ant-CANARY-" + "0123456789abcdefXYZ"

const (
	attemptID = domain.AttemptID("r_t1")
	missionID = domain.MissionID("m_t1")
	fence     = int64(1)
)

func i64(v int64) *int64 { return &v }

// testPrices are arbitrary test numbers, not any vendor's prices.
func testPrices() agent.Prices {
	return agent.Prices{Input: i64(3_000_000), Output: i64(15_000_000), CacheWrite: i64(3_750_000), CacheRead: i64(300_000),
		ServerToolUse: i64(10_000), LongContextThreshold: i64(200_000), LongContextPercent: i64(200)}
}

type crashed struct{ point string }

// fakeTools records executions so a test can prove how many times a tool ran.
type fakeTool struct {
	name string
	side bool
	mu   *sync.Mutex
	runs *[]string
	out  func(in string) string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) SideEffecting() bool { return f.side }
func (f *fakeTool) Def() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"description":"test tool","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}`, f.name))
}
func (f *fakeTool) Run(_ context.Context, in json.RawMessage) (string, bool) {
	f.mu.Lock()
	*f.runs = append(*f.runs, f.name+":"+string(in))
	f.mu.Unlock()
	if f.out != nil {
		return f.out(string(in)), false
	}
	return "ran " + string(in), false
}

type canaryRedactor struct{}

func (canaryRedactor) Redact(s string) string { return strings.ReplaceAll(s, canary, "[REDACTED]") }

type rig struct {
	t      *testing.T
	st     *store.Store
	db     *agent.DB
	srv    *fakeapi.Server
	prov   *anthropic.Provider
	led    *agent.Ledger
	runs   []string
	mu     sync.Mutex
	crash  func(point string)
	cfgMod func(*agent.Config)
	caps   []agent.Cap
}

func newRig(t *testing.T, steps ...fakeapi.Step) *rig {
	t.Helper()
	st := storetest.Open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO missions (id, ticket, state) VALUES ('m_t1', 'goodqa#15040', '{}'::jsonb)`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, host, role) VALUES ('r_t1', 'm_t1', 1, 0, 0, 'RUNNING', 'h_test', 'worker')`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	srv := fakeapi.New(steps...)
	t.Cleanup(srv.Close)
	cl, err := anthropic.New(anthropic.Config{
		BaseURL: srv.URL, Version: "test-version",
		Key:   func(context.Context) (string, error) { return canary, nil },
		Retry: anthropic.RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxRetryAfter: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, db: &agent.DB{Pool: st.Pool()}, srv: srv, prov: &anthropic.Provider{C: cl}}
	r.caps = []agent.Cap{
		{Resource: agent.SpendResource("month", "202611"), Total: 100_000_000},
		{Resource: agent.SpendResource("job", string(missionID)), Total: 5_000_000},
		{Resource: agent.SpendResource("run", string(attemptID)), Total: 1_000_000},
	}
	return r
}

func (r *rig) tools() []agent.Tool {
	return []agent.Tool{
		&fakeTool{name: "echo", side: false, mu: &r.mu, runs: &r.runs},
		&fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs},
	}
}

func (r *rig) loop() *agent.Loop {
	r.t.Helper()
	led := &agent.Ledger{St: r.st, Attempt: attemptID, Fence: fence, Caps: r.caps, TTL: time.Hour}
	r.led = led
	cfg := agent.Config{
		DB: r.db, Ledger: led, Provider: r.prov, Tools: r.tools(), Redact: canaryRedactor{},
		Attempt: attemptID, Fence: fence, Model: "test-model", MaxTokens: 1000, System: "sys", Prompt: "do the task",
		MaxTurns: 10, Wall: time.Hour, RestartCap: 20, Prices: testPrices(), BillingMode: agent.BillingAPIKey,
		Crash: r.crash,
	}
	if r.cfgMod != nil {
		r.cfgMod(&cfg)
	}
	l, err := agent.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return l
}

// life runs one process lifetime; crashed reports an injected crash.
func (r *rig) life() (out agent.Outcome, err error, died *crashed) {
	defer func() {
		if p := recover(); p != nil {
			c, ok := p.(crashed)
			if !ok {
				panic(p)
			}
			died = &c
		}
	}()
	out, err = r.loop().Run(context.Background())
	return
}

func (r *rig) crashOnce(point string) {
	fired := false
	r.crash = func(p string) {
		if p == point && !fired {
			fired = true
			panic(crashed{p})
		}
	}
	r.db.BeforeCommit = nil
}

func (r *rig) spent(resource string) int64 {
	r.t.Helper()
	n, err := r.led.Spent(context.Background(), resource)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) messages() []agent.Message {
	m, err := r.db.Messages(context.Background(), attemptID)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) calls() []agent.ToolCall {
	c, err := r.db.ToolCalls(context.Background(), attemptID)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func toolStep(think, sig, id, name, input string) fakeapi.Step {
	return fakeapi.Step{Events: fakeapi.ToolUse(think, sig, id, name, input, 100, 50)}
}

func endStep(text string) fakeapi.Step { return fakeapi.Step{Events: fakeapi.Text(text, 150, 20)} }

var _ = http.StatusOK

func fakeStop(reason string) fakeapi.Step {
	return fakeapi.Step{Events: fakeapi.StopWith(reason, 10, 5)}
}

func endTail(reason string) []fakeapi.Ev {
	e := fakeapi.StopWith(reason, 1, 1)
	return e[len(e)-2:]
}
