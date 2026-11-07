package native_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
	"fabric/internal/tools"
	"fabric/internal/tools/fakehost"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "native")) }

var canary = "sk-" + "ant-CANARY-" + "0123456789abcdefXYZ"

func hooks(t *testing.T) [][]string {
	d := os.Getenv("FABRIC_HOOKS_DIR")
	if d == "" {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, ".claude", "hooks")
	}
	return [][]string{
		{"python3", filepath.Join(d, "command_guard_hook.py"), "hook"},
		{"python3", filepath.Join(d, "kill_guard.py"), "hook"},
		{"python3", filepath.Join(d, "prod_delete_guard.py")},
	}
}

type env struct {
	g      native.Grant
	marker string
	srv    *fakeapi.Server
}

func newEnv(t *testing.T, steps ...fakeapi.Step) *env {
	t.Helper()
	dir := t.TempDir()
	wt, run := filepath.Join(dir, "wt"), filepath.Join(dir, "run")
	for _, d := range []string{wt, run} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("alpha beta\n"), 0o644)
	marker := filepath.Join(dir, "key-was-read")
	script := filepath.Join(dir, "key.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf '%s\\n' '"+canary+"'\n"), 0o700)
	srv := fakeapi.New(steps...)
	t.Cleanup(srv.Close)
	prices := `{"input_per_mtok_micros":100000,"output_per_mtok_micros":500000,"cache_write_per_mtok_micros":200000,"cache_read_per_mtok_micros":10000,"server_tool_per_use_micros":10000,"long_context_threshold_tokens":100000,"long_context_multiplier_percent":500}`
	g := native.Grant{Provider: "anthropic", Mission: "m_nat1", Attempt: "r_nat1", Ticket: "GoodQA goodqa#15040",
		Model: "test-model", MaxTokens: 400, System: "You are a test agent.", Prompt: "Read notes.txt then echo it.",
		Worktree: wt, RunDir: run, BaseURL: srv.URL, AnthropicVersion: "test-version", KeyCommand: []string{script},
		Prices: json.RawMessage(prices), LiveTest: true, MaxTurns: 6, WallSeconds: 60, RestartCap: 3,
		ShellAllow: []string{`^(ls|cat|echo|wc)( [A-Za-z0-9_./=-]+)*$`}, OracleHooks: hooks(t), RequiredControls: []string{"x"}}
	g.Caps.Month, g.Caps.Job, g.Caps.Run = 100_000_000, 5_000_000, 1_000_000
	return &env{g: g, marker: marker, srv: srv}
}

func opts(contained bool) native.Options {
	return native.Options{Starter: fakehost.New(), Boundary: tools.Boundary{Host: "fake", Contained: contained}}
}

func TestMissionRunEndToEnd(t *testing.T) {
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_r", "read", `{"path":"notes.txt"}`, 120, 40)},
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_s", "shell", `{"command":"echo done"}`, 200, 40)},
		fakeapi.Step{Events: fakeapi.Text("finished", 260, 10)},
	)
	st := storetest.Open(t)
	r, err := native.Run(context.Background(), st, e.g, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != "done" || r.Turns != 3 || len(r.RequestIDs) != 3 || r.SpentMicro <= 0 || r.SpentMicro > 1_000_000 {
		t.Fatalf("receipt %+v", r)
	}
	if r.Billing != "api_key" || r.CostLabel != "billed_estimate_from_usage" {
		t.Fatalf("labels %+v", r)
	}
	// The tool results reached the model.
	reqs := e.srv.Requests()
	if !strings.Contains(string(reqs[1].Body), "alpha beta") || !strings.Contains(string(reqs[2].Body), "done") {
		t.Fatal("tool results missing from follow-up requests")
	}
	// The attempt is finished through the ordinary command path.
	var state string
	if err := st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = 'r_nat1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "RUNNING" {
		t.Fatalf("attempt still RUNNING after the run")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), canary) {
		t.Fatal("key in receipt")
	}
	// Running it again resumes to the same terminal outcome without sending anything.
	n := len(e.srv.Requests())
	r2, err := native.Run(context.Background(), st, e.g, opts(true))
	if err != nil || r2.Outcome != "done" || len(e.srv.Requests()) != n {
		t.Fatalf("rerun: %+v err=%v requests %d->%d", r2, err, n, len(e.srv.Requests()))
	}
}

// Refusals happen before the key is read or a byte is sent.
func TestMissionRunRefusals(t *testing.T) {
	st := storetest.Open(t)
	e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 1, 1)})
	if _, err := native.Run(context.Background(), st, e.g, opts(false)); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("unadvertised host accepted: %v", err)
	}
	e2 := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 1, 1)})
	e2.g.OracleHooks = [][]string{{"true"}} // a blind oracle
	if _, err := native.Run(context.Background(), st, e2.g, opts(true)); err == nil || !strings.Contains(err.Error(), "self-test") {
		t.Fatalf("blind oracle accepted: %v", err)
	}
	for _, e := range []*env{e, e2} {
		if _, err := os.Stat(e.marker); err == nil {
			t.Fatal("the key was read before the boundary and oracle checks passed")
		}
		if len(e.srv.Requests()) != 0 {
			t.Fatal("a request was sent")
		}
	}
	// Caps are fixed in code.
	bad := newEnv(t)
	bad.g.Caps.Month = 200_000_000
	if err := bad.g.Validate(); err == nil {
		t.Error("month cap above USD 100 accepted")
	}
	bad = newEnv(t)
	bad.g.Caps.Job = 6_000_000
	if err := bad.g.Validate(); err == nil {
		t.Error("job cap above USD 5 accepted")
	}
	bad = newEnv(t)
	bad.g.Caps.Run = 2_000_000
	if err := bad.g.Validate(); err == nil {
		t.Error("live test above USD 1 accepted")
	}
	bad = newEnv(t)
	bad.g.Model = ""
	if err := bad.g.Validate(); err == nil {
		t.Error("grant without a model accepted")
	}
}
