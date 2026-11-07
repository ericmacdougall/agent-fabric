package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/domain"
)

func TestRecordedMission(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	if _, err := exec.LookPath("claude"); err == nil {
		t.Fatal("claude still on PATH")
	}
	if _, err := exec.LookPath("codex"); err == nil {
		t.Fatal("codex still on PATH")
	}
	r := newRig(t, toolStep("", "", "toolu_a", "echo", `{"cmd":"hello"}`), endStep("all done"))
	out, err, died := r.life()
	if err != nil || died != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v died=%v", out, err, died)
	}
	if len(r.runs) != 1 || !strings.HasPrefix(r.runs[0], "echo:") {
		t.Fatalf("tool did not run exactly once: %v", r.runs)
	}
	reqs := r.srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %d", len(reqs))
	}
	if !bytes.Contains(reqs[1].Body, []byte(`"tool_use_id":"toolu_a"`)) || !bytes.Contains(reqs[1].Body, []byte(`ran {\"cmd\":\"hello\"}`)) {
		t.Fatalf("tool_result bytes not in the next request: %s", reqs[1].Body)
	}
	if string(reqs[0].Body[:1]) != "{" || !bytes.Contains(reqs[0].Body, []byte(`"model":"test-model"`)) {
		t.Fatal("model must come from the config")
	}
	if len(r.messages()) != 4 {
		t.Fatalf("messages %d", len(r.messages()))
	}
}

// Thinking text, signature and unknown fields round-trip byte-exact (AC-3).
func TestThinkingRoundTrip(t *testing.T) {
	r := newRig(t, toolStep("deep <thought> & more", "SIGNATURE==", "toolu_t", "echo", `{"cmd":"x"}`), endStep("fin"))
	out, err, died := r.life()
	if err != nil || died != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	msgs := r.messages()
	stored := msgs[1].Content // JSON array of the assistant blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(stored, &blocks); err != nil {
		t.Fatal(err)
	}
	second := r.srv.Requests()[1].Body
	for i, b := range blocks {
		if !bytes.Contains(second, b) {
			t.Fatalf("stored block %d bytes missing from the next request:\nblock=%s\nreq=%s", i, b, second)
		}
	}
	// The fake rejects a modified thinking block with a 400; completion proves no drift.
}

func TestAssistantHistoryIsImmutable(t *testing.T) {
	r := newRig(t, endStep("hi"))
	if _, err, _ := r.life(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE agent_messages SET content = '[]'::bytea WHERE role = 'assistant'`,
		`DELETE FROM agent_messages WHERE role = 'assistant'`,
	} {
		if _, err := r.st.Pool().Exec(context.Background(), q); err == nil {
			t.Fatalf("history mutation allowed: %s", q)
		}
	}
}

func TestUnknownStopReasonParks(t *testing.T) {
	r := newRig(t, fakeStop("brand_new_reason"))
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeParked || !strings.Contains(out.Note, "unknown stop_reason") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestMaxTokensRefusalPark(t *testing.T) {
	for _, reason := range []string{"max_tokens", "refusal", "model_context_window_exceeded", "pause_turn"} {
		r := newRig(t, fakeStop(reason))
		out, err, _ := r.life()
		if err != nil || out.Kind != agent.OutcomeParked {
			t.Fatalf("%s: out=%+v err=%v", reason, out, err)
		}
	}
}

// A tool_use block that arrives with a non-tool stop reason is never executed.
func TestToolUseWithoutToolStopNotExecuted(t *testing.T) {
	st := toolStep("", "", "toolu_x", "shell", `{"cmd":"rm -rf /"}`)
	st.Events = append(st.Events[:len(st.Events)-2:len(st.Events)-2], endTail("end_turn")...)
	r := newRig(t, st)
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(r.runs) != 0 {
		t.Fatalf("tool ran without stop_reason tool_use: %v", r.runs)
	}
}

// A guard refusal returns an error result and never runs the tool.
func TestGuardRefusalDoesNotRun(t *testing.T) {
	r := newRig(t, toolStep("", "", "toolu_g", "shell", `{"cmd":"cat ~/.aws/credentials"}`), endStep("ok"))
	r.cfgMod = func(c *agent.Config) { c.Guard = denyAll{} }
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(r.runs) != 0 {
		t.Fatal("refused tool ran")
	}
	if !bytes.Contains(r.srv.Requests()[1].Body, []byte("refused by policy")) {
		t.Fatal("refusal not reported to the model")
	}
}

type denyAll struct{}

func (denyAll) Check(context.Context, string, json.RawMessage) agent.Decision {
	return agent.Decision{Reason: "secret_dump"}
}

func TestToolOutputRedactedAtCreation(t *testing.T) {
	r := newRig(t, toolStep("", "", "toolu_r", "echo", `{"cmd":"`+canary+`"}`), endStep("ok"))
	if _, err, _ := r.life(); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls() {
		if bytes.Contains(c.Result, []byte(canary)) {
			t.Fatal("secret persisted in tool result")
		}
	}
	if bytes.Contains(r.srv.Requests()[1].Body, []byte(canary+"\\\"}")) && bytes.Contains(r.srv.Requests()[1].Body, []byte("ran {\\\"cmd\\\":\\\""+canary)) {
		t.Fatal("secret sent to the model in a tool result")
	}
}

// A superseded runner (older fence) cannot write a message (AC-5 fence).
func TestStaleFenceRefused(t *testing.T) {
	r := newRig(t, endStep("never"))
	if _, err := r.st.Pool().Exec(context.Background(), `UPDATE attempts SET fence = 2 WHERE id = 'r_t1'`); err != nil {
		t.Fatal(err)
	}
	_, err, _ := r.life()
	if !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("err = %v, want ErrStaleFence", err)
	}
	if n := len(r.messages()); n != 0 {
		t.Fatalf("%d messages written by a stale runner", n)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Fatalf("stale runner sent %d requests", n)
	}
}
