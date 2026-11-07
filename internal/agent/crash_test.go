package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
)

// crashAt arranges one crash at the nth call of a named point.
func (r *rig) crashAt(point string, nth int) {
	seen := 0
	r.crash = func(p string) {
		if p != point {
			return
		}
		seen++
		if seen == nth {
			panic(crashed{p})
		}
	}
}

func (r *rig) crashOnCommit(nth int) {
	seen := 0
	r.db.BeforeCommit = func(string) error {
		seen++
		if seen == nth {
			panic(crashed{"commit"})
		}
		return nil
	}
}

func TestCrashResume(t *testing.T) {
	cases := []struct {
		name     string
		arm      func(r *rig)
		steps    int  // model responses consumed in total (crashed requests included)
		shellRun int  // executions of the side-effecting tool
		interr   bool // the tool result must be the synthetic one
	}{
		{"before_request", func(r *rig) { r.crashAt("before_hold", 1) }, 2, 1, false},
		{"after_hold_before_send", func(r *rig) { r.crashAt("after_hold", 1) }, 2, 1, false},
		{"after_send_before_first_byte", func(r *rig) { r.crashAt("mid_stream", 1) }, 3, 1, false},
		{"mid_stream", func(r *rig) { r.crashAt("mid_stream", 4) }, 3, 1, false},
		{"after_tool_use", func(r *rig) { r.crashAt("after_tool_use", 1) }, 2, 1, false},
		{"inside_commit_assistant", func(r *rig) { r.crashOnCommit(3) }, 3, 1, false},
		{"after_mark_executing", func(r *rig) { r.crashAt("after_mark_executing", 1) }, 2, 0, true},
		{"before_finish_tool", func(r *rig) { r.crashAt("before_finish_tool", 1) }, 2, 1, true},
		{"after_tool_result", func(r *rig) { r.crashAt("after_tool_result", 1) }, 2, 1, false},
		{"inside_commit_tool_results", func(r *rig) { r.crashOnCommit(6) }, 2, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			// Script enough tool responses for crashed requests, then the end.
			for i := 0; i < tc.steps-1; i++ {
				r.srv.Push(toolStep("think", "SIG"+fmt.Sprint(i)+"==", "toolu_1", "shell", `{"cmd":"make deploy"}`))
			}
			r.srv.Push(endStep("finished"))
			tc.arm(r)

			_, err, died := r.life()
			if died == nil {
				t.Fatalf("crash %s never fired (err=%v)", tc.name, err)
			}
			r.crash, r.db.BeforeCommit = nil, nil // the next process life has no fault
			out, err, died2 := r.life()
			if err != nil || died2 != nil || out.Kind != agent.OutcomeDone {
				t.Fatalf("resume: out=%+v err=%v died=%v", out, err, died2)
			}

			msgs := r.messages()
			if len(msgs) != 4 {
				t.Fatalf("history has %d messages, want 4", len(msgs))
			}
			for i, m := range msgs {
				if m.Idx != i {
					t.Fatalf("history gap at %d", i)
				}
			}
			calls := r.calls()
			if len(calls) != 1 {
				t.Fatalf("tool_use rows %d, want exactly 1 (no duplicate ids)", len(calls))
			}
			shell := 0
			for _, s := range r.runs {
				if strings.HasPrefix(s, "shell:") {
					shell++
				}
			}
			if shell != tc.shellRun {
				t.Fatalf("side-effecting tool ran %d times, want %d (runs=%v)", shell, tc.shellRun, r.runs)
			}
			if (calls[0].State == "INTERRUPTED") != tc.interr || (tc.interr && !strings.Contains(string(calls[0].Result), "interrupted")) {
				t.Fatalf("call state %s result %q, interrupted wanted=%v", calls[0].State, calls[0].Result, tc.interr)
			}
			if got := len(r.srv.Requests()); got != tc.steps {
				t.Fatalf("requests %d, want %d", got, tc.steps)
			}
			// Every hold is settled or reaped: nothing stays held after a finished run.
			res, _ := r.st.Reservations(context.Background(), attemptID)
			for _, v := range res {
				if v.State == store.ResHeld {
					t.Fatalf("hold %s still HELD after the run ended", v.ID)
				}
			}
			// The tool_result sent on the last request is the recorded one.
			var blocks []json.RawMessage
			_ = json.Unmarshal(msgs[2].Content, &blocks)
			if !strings.Contains(string(r.srv.Requests()[tc.steps-1].Body), string(blocks[0])) {
				t.Fatalf("final request does not replay the recorded tool_result")
			}
		})
	}
}

// Partial SSE never executes a tool, even when the tool_use block is complete.
func TestPartialStreamNeverExecutes(t *testing.T) {
	bad := toolStep("", "", "toolu_bad", "shell", `{"cmd":"rm -rf build"}`)
	bad.CutAfter = 7 // delivered through the tool_use content_block_stop, no message_delta/stop
	r := newRig(t, bad, toolStep("", "", "toolu_ok", "shell", `{"cmd":"ls"}`), endStep("fin"))
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	for _, s := range r.runs {
		if strings.Contains(s, "rm -rf") {
			t.Fatalf("a tool_use from a truncated stream was executed: %v", r.runs)
		}
	}
	if len(r.runs) != 1 {
		t.Fatalf("runs %v", r.runs)
	}
}

// A crash loop is stopped by the restart cap, and what it can spend is
// bounded by the holds its lives took.
func TestCrashLoopBreaker(t *testing.T) {
	r := newRig(t, endStep("x"), endStep("x"), endStep("x"), endStep("x"), endStep("x"))
	r.cfgMod = func(c *agent.Config) { c.RestartCap = 3 }
	r.crash = func(p string) {
		if p == "after_hold" {
			panic(crashed{p})
		}
	}
	var last agent.Outcome
	crashes := 0
	for i := 0; i < 6; i++ {
		out, err, died := r.life()
		if died != nil {
			crashes++
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		last = out
		break
	}
	if crashes != 3 || last.Kind != agent.OutcomeParked || !strings.Contains(last.Note, "restart cap") {
		t.Fatalf("crashes=%d last=%+v", crashes, last)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Fatalf("%d requests sent by a crash loop that dies before sending", n)
	}
	res, _ := r.st.Reservations(context.Background(), attemptID)
	perResource := 0
	for _, v := range res {
		if v.Resource == r.caps[2].Resource {
			perResource++
		}
	}
	if perResource > 3 {
		t.Fatalf("%d holds from 3 lives", perResource)
	}
}

// F2: a request that was written and then lost is charged at the held amount.
func TestTransportAfterWriteCharged(t *testing.T) {
	r := newRig(t, fakeapi.Step{DieBeforeBody: true}, endStep("ok"))
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	reqs := r.srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %d", len(reqs))
	}
	want := testPrices().WorstCase(len(reqs[0].Body), 1000) + testPrices().Cost(agent.Usage{Input: 150, Output: 20})
	if got := r.spent(r.caps[2].Resource); got != want {
		t.Fatalf("spent %d, want held %d for the lost request plus actual", got, want)
	}
}

// F3: a second runner on a live attempt is refused and disturbs nothing.
func TestSecondRunnerRefused(t *testing.T) {
	r := newRig(t, endStep("one"))
	hold := make(chan struct{})
	reached := make(chan struct{})
	r.crash = func(p string) {
		if p == "after_hold" {
			close(reached)
			<-hold
		}
	}
	done := make(chan agent.Outcome, 1)
	go func() { o, _, _ := r.life(); done <- o }()
	<-reached
	r.crash = nil // the first loop already captured its own hook
	_, err, _ := r.life()
	if !errors.Is(err, agent.ErrAttemptBusy) {
		t.Fatalf("second runner: %v", err)
	}
	close(hold)
	if o := <-done; o.Kind != agent.OutcomeDone {
		t.Fatalf("first runner disturbed: %+v", o)
	}
	res, _ := r.st.Reservations(context.Background(), attemptID)
	if len(res) != len(r.caps) {
		t.Fatalf("second runner touched the holds: %d reservations", len(res))
	}
}

// F5: a resume under a different configuration is refused.
func TestConfigChangeRefused(t *testing.T) {
	r := newRig(t, endStep("a"), endStep("b"))
	r.crashAt("after_hold", 1)
	if _, _, died := r.life(); died == nil {
		t.Fatal("no crash")
	}
	r.crash = nil
	r.cfgMod = func(c *agent.Config) { c.System = "a different system prompt" }
	if _, err, _ := r.life(); !errors.Is(err, agent.ErrConfigChanged) {
		t.Fatalf("err=%v, want ErrConfigChanged", err)
	}
}
