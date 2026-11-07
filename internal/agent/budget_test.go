package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
)

// The hold exists, committed, at the moment the request reaches the server.
func TestBudgetHeldBeforeSend(t *testing.T) {
	r := newRig(t, endStep("hi"))
	r.srv.Validate = func([]byte) error {
		res, err := r.st.Reservations(context.Background(), attemptID)
		if err != nil {
			return err
		}
		held := 0
		for _, v := range res {
			if v.State == store.ResHeld {
				held++
			}
		}
		if held != len(r.caps) {
			return fmt.Errorf("no durable hold when the request arrived (%d held)", held)
		}
		return nil
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestBudgetExhaustedSendsNothing(t *testing.T) {
	r := newRig(t, endStep("never"))
	r.caps[2].Total = 100 // micro-USD: smaller than any worst-case request
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeBudgetExhausted {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Fatalf("%d requests sent past an exhausted budget", n)
	}
}

// Cumulative spend is remembered: once a cap is nearly used, the next hold refuses.
func TestBudgetCumulative(t *testing.T) {
	script := func() []fakeapi.Step {
		return []fakeapi.Step{toolStep("", "", "toolu_a", "echo", `{"cmd":"a"}`), endStep("fin")}
	}
	probe := newRig(t, script()...)
	if _, err, _ := probe.life(); err != nil {
		t.Fatal(err)
	}
	hold2 := testPrices().WorstCase(len(probe.srv.Requests()[1].Body), 1000)
	actual1 := testPrices().Cost(agent.Usage{Input: 100, Output: 50})

	r := newRig(t, script()...)
	// Room for either hold alone, but not for hold2 once actual1 has been spent:
	// only a ledger that remembers spend refuses the second turn.
	r.caps[2].Total = hold2 + actual1/2
	out, err, _ := r.life()
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != agent.OutcomeBudgetExhausted {
		t.Fatalf("second turn should have been refused: %+v (requests=%d)", out, len(r.srv.Requests()))
	}
	if n := len(r.srv.Requests()); n != 1 {
		t.Fatalf("requests %d, want 1", n)
	}
}

// Every price field is exercised: input, output (thinking is output), cache write and read.
func TestBudgetUsageCosts(t *testing.T) {
	r := newRig(t, fakeapi.Step{Events: fakeapi.WithCache("answer", 1000, 500, 2000, 4000)})
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	// 1000*3 + 500*15 + 2000*3.75 + 4000*0.3 per million, in micro-USD.
	const want = 3000 + 7500 + 7500 + 1200
	for _, c := range r.caps {
		if got := r.spent(c.Resource); got != want {
			t.Fatalf("%s spent %d, want %d", c.Resource, got, want)
		}
	}
}

func TestPriceConfigFailsClosed(t *testing.T) {
	full := `{"input_per_mtok_micros":1,"output_per_mtok_micros":1,"cache_write_per_mtok_micros":1,"cache_read_per_mtok_micros":1,"server_tool_per_use_micros":1,"long_context_threshold_tokens":1000,"long_context_multiplier_percent":200}`
	if _, err := agent.ParsePrices([]byte(full)); err != nil {
		t.Fatalf("complete config refused: %v", err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(full), &m)
	for k := range m {
		c := map[string]json.RawMessage{}
		for k2, v := range m {
			if k2 != k {
				c[k2] = v
			}
		}
		b, _ := json.Marshal(c)
		if _, err := agent.ParsePrices(b); err == nil {
			t.Errorf("config without %s accepted", k)
		}
	}
	if _, err := agent.ParsePrices([]byte(`{"input_per_mtok_micros":1,"surprise":2}`)); err == nil {
		t.Error("unknown field accepted")
	}
	r := newRig(t, endStep("x"))
	r.cfgMod = func(c *agent.Config) { c.Prices = agent.Prices{} }
	if _, err := agent.New(agent.Config{DB: r.db, Ledger: &agent.Ledger{}, Provider: r.prov, Redact: canaryRedactor{}, Model: "m", MaxTokens: 1, MaxTurns: 1, Wall: 1}); err == nil {
		t.Fatal("loop built with an empty price table")
	}
}

// A hold left by a crash after the request was sent is charged at the held amount.
func TestHoldSurvivesCrash(t *testing.T) {
	r := newRig(t, endStep("one"), endStep("two"))
	r.crashAt("mid_stream", 2)
	if _, _, died := r.life(); died == nil {
		t.Fatal("no crash")
	}
	r.crash = nil
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	reqs := r.srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %d", len(reqs))
	}
	held := testPrices().WorstCase(len(reqs[0].Body), 1000)
	actual := testPrices().Cost(agent.Usage{Input: 150, Output: 20})
	if got, want := r.spent(r.caps[2].Resource), held+actual; got != want {
		t.Fatalf("spent %d, want held %d + actual %d = %d", got, held, actual, want)
	}
}

// Billing labels (AC-27, native half).
func TestBillingLabels(t *testing.T) {
	r := newRig(t, endStep("x"))
	if _, err, _ := r.life(); err != nil {
		t.Fatal(err)
	}
	var mode, label string
	var cost int64
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT r.billing_mode, m.cost_label, m.cost_micros FROM agent_runs r JOIN agent_messages m USING (attempt_id) WHERE m.role = 'assistant'`).Scan(&mode, &label, &cost); err != nil {
		t.Fatal(err)
	}
	if mode != agent.BillingAPIKey || label != agent.LabelAPIEstimate || cost <= 0 {
		t.Fatalf("mode=%s label=%s cost=%d", mode, label, cost)
	}
	r2 := newRig(t, endStep("x"))
	r2.cfgMod = func(c *agent.Config) { c.BillingMode = agent.BillingSubscription }
	r2.t = t
	func() {
		defer func() { _ = recover() }()
		if _, err := agent.New(agent.Config{DB: r2.db, Ledger: &agent.Ledger{}, Provider: r2.prov, Redact: canaryRedactor{}, Model: "m", MaxTokens: 1, MaxTurns: 1, Wall: 1, Prices: testPrices(), BillingMode: agent.BillingSubscription}); err == nil {
			t.Fatal("subscription billing accepted in slice 1")
		}
	}()
}

// The key never reaches stdout, the database, request recordings or errors (AC-8).
func TestKeyNeverLeaks(t *testing.T) {
	r := newRig(t,
		fakeapi.Step{Status: 529, ErrType: "overloaded_error", ErrMessage: "busy"},
		toolStep("", "", "toolu_k", "echo", `{"cmd":"hi"}`),
		endStep("fin"))
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	var dump strings.Builder
	for _, tbl := range []string{"agent_runs", "agent_messages", "agent_tool_calls", "reservations", "usage_receipts", "hold_commands", "attempts", "missions"} {
		rows, err := r.st.Pool().Query(context.Background(), "SELECT row_to_json(t)::text FROM "+tbl+" t")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			dump.WriteString(s)
		}
		rows.Close()
	}
	for _, rec := range r.srv.Requests() {
		dump.Write(rec.Body) // request recordings are bodies; headers are never persisted
	}
	if strings.Contains(dump.String(), canary) || strings.Contains(dump.String(), "sk-"+"ant-") {
		t.Fatal("api key found in persisted artifacts")
	}
	if strings.Contains(dump.String(), "x-api-key") {
		t.Fatal("request headers found in persisted artifacts")
	}
}

// No model name appears in non-test source; a planted name proves the scan works (AC-6).
func TestNoModelNamesInSource(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)claude-(opus|sonnet|haiku|fable|mythos|instant|[0-9])`),
		regexp.MustCompile(`(?i)\bgpt-[0-9a-z]`),
		regexp.MustCompile(`(?i)\bo[1-9]-(mini|pro|preview)\b`),
	}
	scan := func(root string) []string {
		var hits []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/testdata/") {
				return nil
			}
			b, _ := os.ReadFile(p)
			for _, re := range patterns {
				if re.Match(b) {
					hits = append(hits, p)
				}
			}
			return nil
		})
		return hits
	}
	dir := t.TempDir()
	planted := filepath.Join(dir, "planted.go")
	_ = os.WriteFile(planted, []byte(`package x; const m = "claude-`+`sonnet-9"`), 0o644)
	if len(scan(dir)) != 1 {
		t.Fatal("scan did not detect a planted model name")
	}
	if hits := scan("../../internal"); len(hits) > 0 {
		t.Fatalf("model names in source: %v", hits)
	}
	if hits := scan("../../cmd"); len(hits) > 0 {
		t.Fatalf("model names in cmd: %v", hits)
	}
}
