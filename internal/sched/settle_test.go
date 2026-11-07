package sched

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/domain"
)

const (
	initEv   = `{"type":"system","subtype":"init","session_id":"s1","model":"fake-model"}`
	initEv2  = `{"type":"system","subtype":"init","session_id":"s1","model":"other-model"}`
	okResult = `{"type":"result","subtype":"success","session_id":"s1","uuid":"r1","num_turns":1,"is_error":false,"structured_output":{"end":{"kind":"checkpoint"}}}`
	badRes   = `{"type":"result","subtype":"error_max_budget_usd","session_id":"s1","uuid":"r0","num_turns":1,"is_error":true}`
)

// settleCase starts and acks one attempt, writes its stream and settles it
// with the given exit code. It returns the attempt state and whether a
// verify command was recorded.
func settleCase(t *testing.T, id string, code int, lines ...string) (string, bool) {
	t.Helper()
	m := domain.MissionID("m_" + id)
	a := domain.AttemptID("r_" + id + "_1")
	st, ln, cfg := ownerSetup(t, m)
	ctx := context.Background()
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(st, cfg, ln)
	var werr error
	if code < 0 { // a failed Wait: the exit is not known
		code, werr = 0, errors.New("wait: no child")
	}
	if err := s.afterWait(ctx, m, a, fence, dir, code, werr); err != nil {
		t.Logf("settle: %v", err)
	}
	var state string
	var verified int
	st.Pool().QueryRow(ctx, "SELECT state FROM attempts WHERE id = $1", string(a)).Scan(&state)                         //nolint:errcheck
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM commands WHERE command_id = $1", "verify:"+string(a)).Scan(&verified) //nolint:errcheck
	return state, verified > 0
}

// goodqa#15082 T1 and stream settlement: a run is VERIFIED only when the
// process exited 0 and its stream is one coherent init then one success.
func TestSettleNeedsCleanExitAndCoherentStream(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		lines  []string
		verify bool
	}{
		{"clean", 0, []string{initEv, okResult}, true},
		{"nonzero exit, success stream", 1, []string{initEv, okResult}, false},
		{"wait failed, success stream", -1, []string{initEv, okResult}, false},
		{"failed result then success", 0, []string{initEv, badRes, okResult}, false},
		{"result then init", 0, []string{okResult, initEv}, false},
		{"init, result, init", 0, []string{initEv, okResult, initEv}, false},
		{"model changes", 0, []string{initEv, initEv2, okResult}, false},
		{"duplicate result", 0, []string{initEv, okResult, okResult}, false},
	}
	for i, c := range cases {
		state, verified := settleCase(t, "st"+string(rune('a'+i)), c.code, c.lines...)
		if verified != c.verify || (c.verify != (state == "VERIFIED")) {
			t.Errorf("%s: state %s, verify recorded %v; want verify %v", c.name, state, verified, c.verify)
		}
	}
}
