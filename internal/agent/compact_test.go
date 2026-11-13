package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic/fakeapi"
)

func bigShell(r *rig) agent.Tool {
	return &fakeTool{name: "shell", side: false, mu: &r.mu, runs: &r.runs, out: func(string) string { return strings.Repeat("x", 12000) }}
}

func clientCompaction(r *rig, trigger int) {
	r.cfgMod = func(c *agent.Config) {
		c.Tools = []agent.Tool{bigShell(r)}
		c.Compaction = agent.CompactionConfig{Mode: agent.CompactClient, WindowTokens: 8000, TriggerTokens: trigger, OutputCap: 1024}
	}
}

func shellTurn(id string) fakeapi.Step { return toolStep("", "", id, "shell", `{"cmd":"go"}`) }

func (r *rig) sentMessages(i int) []struct {
	Role    string
	Content []json.RawMessage
} {
	r.t.Helper()
	var req struct {
		Messages []struct {
			Role    string
			Content []json.RawMessage
		}
	}
	if err := json.Unmarshal(r.srv.Requests()[i].Body, &req); err != nil {
		r.t.Fatal(err)
	}
	return req.Messages
}

func (r *rig) compactions() []agent.Compaction {
	r.t.Helper()
	c, err := r.db.Compactions(context.Background(), attemptID)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *rig) kindOf(out agent.Outcome) string { return out.Kind + ": " + out.Note }

// AC-6: a trigger, and separately an overflow stop, each make exactly one
// summary request and one row; the next request is the summary then the kept
// history; the mission ends.
func TestCompactClientSide(t *testing.T) {
	cases := map[string]struct {
		steps        []fakeapi.Step
		trigger      int
		wantIdx      int
		wantFirst    int
		summaryAt    int
		keptRoleHead string
	}{
		"trigger": {
			steps:   []fakeapi.Step{shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), endStep("SUMMARY-1"), endStep("all done")},
			trigger: 8000, wantIdx: 7, wantFirst: 5, summaryAt: 3,
		},
		"overflow stop": {
			steps:   []fakeapi.Step{shellTurn("toolu_c1"), shellTurn("toolu_c2"), fakeStop("model_context_window_exceeded"), endStep("SUMMARY-1"), endStep("all done")},
			trigger: 0, wantIdx: 6, wantFirst: 3, summaryAt: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tc.steps...)
			clientCompaction(r, tc.trigger)
			out, err, died := r.life()
			if err != nil || died != nil || out.Kind != agent.OutcomeDone {
				t.Fatalf("%v %v %s", err, died, r.kindOf(out))
			}
			cs := r.compactions()
			if len(cs) != 1 || cs[0].Idx != tc.wantIdx || cs[0].FirstKeptIdx != tc.wantFirst || cs[0].Summary != "SUMMARY-1" {
				t.Fatalf("rows %+v", cs)
			}
			if n := len(r.srv.Requests()); n != len(tc.steps) {
				t.Fatalf("%d requests, want %d (exactly one summary request)", n, len(tc.steps))
			}
			sum := r.sentMessages(tc.summaryAt)
			if len(sum) != 1 || strings.Contains(string(r.srv.Requests()[tc.summaryAt].Body), `"tools"`) {
				t.Fatalf("the summary request must be one message with no tools: %s", r.srv.Requests()[tc.summaryAt].Body)
			}
			next := r.sentMessages(tc.summaryAt + 1)
			if len(next) < 2 || next[0].Role != "user" || !strings.Contains(string(next[0].Content[0]), "SUMMARY-1") || next[1].Role != "assistant" {
				t.Fatalf("the request after compaction must be summary then the kept assistant message: %s", r.srv.Requests()[tc.summaryAt+1].Body)
			}
			if last := next[len(next)-1]; last.Role != "user" {
				t.Fatalf("the retry must end on a user message, got %s", last.Role)
			}
		})
	}
}

// AC-7: a resume after the commit makes no new summary request and builds a
// byte-identical request.
func TestCompactReplayStable(t *testing.T) {
	steps := []fakeapi.Step{shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), endStep("SUMMARY-1"), endStep("all done")}
	a := newRig(t, steps...)
	clientCompaction(a, 8000)
	if out, err, _ := a.life(); err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, a.kindOf(out))
	}
	want := a.srv.Requests()[4].Body
	nWant := len(a.srv.Requests())

	b := newRig(t, steps...)
	clientCompaction(b, 8000)
	b.crashOnce("after_compaction_commit")
	if _, _, died := b.life(); died == nil {
		t.Fatal("no crash")
	}
	if len(b.compactions()) != 1 {
		t.Fatal("the row must be committed before the crash point")
	}
	out, err, _ := b.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, b.kindOf(out))
	}
	if n := len(b.srv.Requests()); n != nWant {
		t.Fatalf("a resume re-ran the summary: %d requests, want %d", n, nWant)
	}
	if string(b.srv.Requests()[4].Body) != string(want) {
		t.Fatal("the request after a resume is not byte-identical")
	}
}

// NEG-13: a crash after the summary Send and before the commit re-runs the
// summary once and double-settles nothing.
func TestCompactCrash(t *testing.T) {
	steps := []fakeapi.Step{shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), endStep("SUMMARY-1"), endStep("SUMMARY-2"), endStep("all done")}
	r := newRig(t, steps...)
	clientCompaction(r, 8000)
	r.crashOnce("after_summary_send")
	if _, _, died := r.life(); died == nil {
		t.Fatal("no crash")
	}
	if len(r.compactions()) != 0 {
		t.Fatal("a row exists before the commit")
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	cs := r.compactions()
	if len(cs) != 1 || cs[0].Summary != "SUMMARY-2" {
		t.Fatalf("rows %+v", cs)
	}
	if n := len(r.srv.Requests()); n != 6 {
		t.Fatalf("%d requests, want 6 (the summary re-run exactly once)", n)
	}
	var want int64
	for _, u := range []agent.Usage{{Input: 100, Output: 50}, {Input: 100, Output: 50}, {Input: 100, Output: 50}, {Input: 150, Output: 20}, {Input: 150, Output: 20}, {Input: 150, Output: 20}} {
		want += testPrices().Cost(u)
	}
	if got := r.spent(agent.SpendResource("run", string(attemptID))); got != want {
		t.Fatalf("spent %d, want %d (a settle was doubled or lost)", got, want)
	}
	// the crash after a commit makes no request: a second resume of a finished run is a no-op
	before := len(r.srv.Requests())
	if _, err, _ := r.life(); err != nil || len(r.srv.Requests()) != before {
		t.Fatalf("finished run was sent again: %v", err)
	}
}

// NEG-11: cut rules.
func TestCompactCutRules(t *testing.T) {
	// an oversized prefix is cut from its oldest side for the summary input
	steps := []fakeapi.Step{shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), shellTurn("toolu_c4"), shellTurn("toolu_c5"), shellTurn("toolu_c6"),
		fakeStop("model_context_window_exceeded"), endStep("SUMMARY-1"), endStep("done")}
	r := newRig(t, steps...)
	clientCompaction(r, 0)
	if out, err, _ := r.life(); err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	cs := r.compactions()
	if len(cs) < 1 {
		t.Fatal("no compaction")
	}
	for _, c := range cs {
		for _, m := range r.messages() {
			if m.Idx == c.FirstKeptIdx && m.Role != "assistant" {
				t.Fatalf("the cut starts at a %s message", m.Role)
			}
		}
	}
	found := false
	for i := range r.srv.Requests() {
		if strings.Contains(string(r.srv.Requests()[i].Body), "[earlier history omitted]") {
			found = true
		}
	}
	if !found {
		t.Fatal("an oversized prefix was not cut from its oldest side")
	}
	// no valid cut: every assistant+result pair is bigger than half the window
	huge := func(r *rig) {
		r.cfgMod = func(c *agent.Config) {
			c.Tools = []agent.Tool{&fakeTool{name: "shell", mu: &r.mu, runs: &r.runs, out: func(string) string { return strings.Repeat("y", 20000) }}}
			c.Compaction = agent.CompactionConfig{Mode: agent.CompactClient, WindowTokens: 8000, TriggerTokens: 8000, OutputCap: 1024}
		}
	}
	r2 := newRig(t, shellTurn("toolu_h1"), shellTurn("toolu_h2"), shellTurn("toolu_h3"))
	huge(r2)
	out, err, _ := r2.life()
	if err != nil || out.Kind != agent.OutcomeParked || !strings.Contains(out.Note, "no valid cut") {
		t.Fatalf("%v %s", err, r2.kindOf(out))
	}
	// the trigger fires before the third turn: two turns were sent, no summary
	if n := len(r2.srv.Requests()); n != 2 {
		t.Fatalf("a summary was requested with no valid cut: %d requests", n)
	}
}

// NEG-12: one compact-and-retry only.
func TestCompactOnce(t *testing.T) {
	r := newRig(t, shellTurn("toolu_c1"), shellTurn("toolu_c2"), fakeStop("model_context_window_exceeded"), endStep("SUMMARY-1"), fakeStop("model_context_window_exceeded"), endStep("never"))
	clientCompaction(r, 0)
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeParked || !strings.Contains(out.Note, "after compaction") {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	if n := len(r.srv.Requests()); n != 5 {
		t.Fatalf("%d requests: a third summary was made", n)
	}
	if len(r.compactions()) != 1 {
		t.Fatalf("rows %+v", r.compactions())
	}
}

// A 400 prompt-too-long compacts and retries too, and the second one parks.
func TestCompactOnPromptTooLong(t *testing.T) {
	tooLong := fakeapi.Step{Status: 400, ErrType: "invalid_request_error", ErrMessage: "prompt is too long: 250000 tokens > 200000 maximum"}
	r := newRig(t, shellTurn("toolu_c1"), shellTurn("toolu_c2"), tooLong, endStep("SUMMARY-1"), endStep("all done"))
	clientCompaction(r, 0)
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone || len(r.compactions()) != 1 {
		t.Fatalf("%v %s %+v", err, r.kindOf(out), r.compactions())
	}
}

// NEG-19: a summary hold that cannot be taken parks as budget-exhausted and
// sends nothing.
func TestCompactSummaryHoldRefused(t *testing.T) {
	r := newRig(t, shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), endStep("SUMMARY-1"), endStep("never"))
	r.caps[2].Total = 250_000
	r.cfgMod = func(c *agent.Config) {
		c.Tools = []agent.Tool{bigShell(r)}
		c.Compaction = agent.CompactionConfig{Mode: agent.CompactClient, WindowTokens: 8000, TriggerTokens: 8000, OutputCap: 16384}
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeBudgetExhausted {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	if n := len(r.srv.Requests()); n != 3 {
		t.Fatalf("a summary request was sent without its hold: %d requests", n)
	}
	if len(r.compactions()) != 0 {
		t.Fatal("a row was written")
	}
}

// AC-8 hold formula: WorstCase(body, max_tokens) + WorstCase(body, output cap).
func TestCompactionHoldFormula(t *testing.T) {
	probe := newRig(t, endStep("hi"))
	probe.cfgMod = func(c *agent.Config) {
		c.Compaction = agent.CompactionConfig{Mode: agent.CompactServer, OutputCap: 700}
	}
	if out, err, _ := probe.life(); err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, probe.kindOf(out))
	}
	body := len(probe.srv.Requests()[0].Body)
	p := testPrices()
	want := p.WorstCase(body, 1000, 0) + p.WorstCase(body, 700, 0)
	for name, c := range map[string]struct {
		cap  int64
		sent bool
	}{"hold exactly fits": {want, true}, "one micro short": {want - 1, false}} {
		r := newRig(t, endStep("hi"))
		r.caps[2].Total = c.cap
		r.cfgMod = func(cf *agent.Config) {
			cf.Compaction = agent.CompactionConfig{Mode: agent.CompactServer, OutputCap: 700}
		}
		out, err, _ := r.life()
		if err != nil {
			t.Fatal(err)
		}
		if got := len(r.srv.Requests()) == 1; got != c.sent {
			t.Fatalf("%s: sent=%v (%s)", name, got, r.kindOf(out))
		}
	}
}

// NEG-17: a fixture whose usage.iterations total exceeds the hold parks through
// budgetViolation and the full actual cost is settled.
func TestCompactionHoldOverrun(t *testing.T) {
	r := newRig(t, fakeapi.Step{Events: fakeapi.Compacted("SUM", "done", 100, 20, 100_000, 2000)})
	r.cfgMod = func(c *agent.Config) {
		c.Compaction = agent.CompactionConfig{Mode: agent.CompactServer, OutputCap: 100}
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeBudgetExhausted || !strings.Contains(out.Note, "exceeded held reservation") {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	actual := testPrices().Cost(agent.Usage{Input: 100, Output: 20, Iterations: []agent.UsageIteration{
		{Type: "compaction", Input: 100_000, Output: 2000}, {Type: "message", Input: 100, Output: 20}}})
	if got := r.spent(agent.SpendResource("run", string(attemptID))); got != actual {
		t.Fatalf("settled %d, want the full actual %d", got, actual)
	}
}

// The compaction block is stored verbatim and the request after it omits what
// came before it; a null summary omits nothing.
func TestCompactionServerReplay(t *testing.T) {
	r := newRig(t, toolStep("", "", "toolu_s1", "echo", `{"cmd":"a"}`),
		fakeapi.Step{Events: toolWithCompaction("toolu_s2", "SERVER-SUMMARY")}, endStep("fin"))
	r.cfgMod = func(c *agent.Config) {
		c.Compaction = agent.CompactionConfig{Mode: agent.CompactServer, OutputCap: 1000}
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	third := r.sentMessages(2)
	if third[0].Role != "assistant" || !strings.Contains(string(third[0].Content[0]), "SERVER-SUMMARY") {
		t.Fatalf("the request after the compaction block must start at it: %s", r.srv.Requests()[2].Body)
	}
	if len(third) != 2 {
		t.Fatalf("pre-compaction messages were not omitted: %d messages", len(third))
	}
}

func toolWithCompaction(id, summary string) []fakeapi.Ev {
	return fakeapi.CompactedToolUse(summary, id, "echo", `{"cmd":"b"}`, 100, 20, 1000, 100)
}

// NEG-20 (main's requirement): a summary that comes back empty fails safe. The
// history is kept with no cut, no agent_compactions row is written, the event is
// recorded as the park note, and nothing crashes.
func TestCompactEmptySummary(t *testing.T) {
	for name, summary := range map[string]string{"empty": "", "blank": "  \n "} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, shellTurn("toolu_c1"), shellTurn("toolu_c2"), shellTurn("toolu_c3"), endStep(summary), endStep("never"))
			clientCompaction(r, 8000)
			before := len(r.messages())
			out, err, died := r.life()
			if err != nil || died != nil {
				t.Fatalf("crash or error: %v %v", err, died)
			}
			if out.Kind != agent.OutcomeParked || !strings.Contains(out.Note, "no usable summary") {
				t.Fatalf("not recorded as a park: %s", r.kindOf(out))
			}
			if cs := r.compactions(); len(cs) != 0 {
				t.Fatalf("a compaction row was written for an empty summary: %+v", cs)
			}
			if got := len(r.messages()); got != before+0 && got != 7 {
				t.Fatalf("history changed: %d messages", got)
			}
			if n := len(r.srv.Requests()); n != 4 {
				t.Fatalf("%d requests: the loop went on after an empty summary", n)
			}
		})
	}
}

// A suffix that starts at a tool_result (user) message is never a cut point: when
// only such a suffix fits the kept budget, there is no valid cut and nothing is
// summarised.
func TestCompactNoCutAtUserMessage(t *testing.T) {
	bigInput := `{"cmd":"` + strings.Repeat("z", 5000) + `"}`
	r := newRig(t, toolStep("", "", "toolu_u1", "shell", bigInput), toolStep("", "", "toolu_u2", "shell", bigInput), endStep("SUMMARY-1"), endStep("never"))
	clientCompaction(r, 8000)
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeParked || !strings.Contains(out.Note, "no valid cut") {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	if len(r.compactions()) != 0 || len(r.srv.Requests()) != 2 {
		t.Fatalf("a cut was taken at a user message: rows %+v, %d requests", r.compactions(), len(r.srv.Requests()))
	}
}
