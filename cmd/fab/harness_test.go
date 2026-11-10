package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

func TestPassEnvIsAllowListed(t *testing.T) {
	t.Setenv("FABRIC_DSN", "dsn-canary-value-1234")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-canary-value-1234")
	for _, bad := range []string{"FABRIC_DSN", "FABRIC_REVIEW_DSN", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "path", "USER,FABRIC_DSN", "A B", "HOME;X"} {
		if _, _, err := parsePassEnv(bad); err == nil {
			t.Errorf("--pass-env %q accepted", bad)
		}
	}
	t.Setenv("USER", "someone")
	t.Setenv("ANTHROPIC_API_KEY", "key-canary-value-1234")
	env, secrets, err := parsePassEnv("USER, ANTHROPIC_API_KEY,LANG")
	if err != nil {
		t.Fatal(err)
	}
	if env["USER"] != "someone" || env["ANTHROPIC_API_KEY"] == "" {
		t.Fatalf("env %v", env)
	}
	if len(secrets) != 1 || secrets[0] != "key-canary-value-1234" {
		t.Fatalf("credential values not reported for masking: %v", secrets)
	}
}

func emitAll(t *testing.T, build func(em *harness.Emitter)) []harness.Event {
	t.Helper()
	rec := &harnesstest.Recorder{}
	em, _ := harness.NewEmitter("claude-code", "ses_j", rec, harness.PatternRedactor())
	build(em)
	return rec.Snapshot()
}

func TestJudgeOutcomes(t *testing.T) {
	ctx := context.Background()
	reply := func(em *harness.Emitter) { _ = em.Message(ctx, "assistant", "hi") }
	cases := []struct {
		name    string
		adapter string
		final   harness.Final
		extra   func(*harness.Emitter)
		wantErr bool
	}{
		{"clean claude stop", "claude-code", harness.Final{Outcome: harness.OutcomeStopped}, reply, false},
		{"claude crash", "claude-code", harness.Final{Outcome: harness.OutcomeFailed, Reason: "exited without a result", ExitCode: 1}, nil, true},
		{"claude stopped nonzero", "claude-code", harness.Final{Outcome: harness.OutcomeStopped, ExitCode: 7}, reply, true},
		{"claude undelivered", "claude-code", harness.Final{Outcome: harness.OutcomeStopped, Reason: "undelivered_inputs=1"}, reply, true},
		{"claude no reply", "claude-code", harness.Final{Outcome: harness.OutcomeStopped}, nil, true},
		{"claude turn error", "claude-code", harness.Final{Outcome: harness.OutcomeStopped}, func(em *harness.Emitter) {
			reply(em)
			_ = em.Message(ctx, "system", "turn ended: error_during_execution")
		}, true},
		{"claude merged note is ok", "claude-code", harness.Final{Outcome: harness.OutcomeStopped, Reason: "inputs=3 results=2 (queued inputs may have been merged by the harness, or lost)"}, reply, false},
		{"native done", "native", harness.Final{Outcome: harness.OutcomeDone, ExitCode: -1}, nil, false},
		{"native failed", "native", harness.Final{Outcome: harness.OutcomeFailed, Reason: "max_turns"}, nil, true},
		{"native stopped", "native", harness.Final{Outcome: harness.OutcomeStopped}, nil, true},
	}
	for _, c := range cases {
		evs := emitAll(t, func(em *harness.Emitter) {
			if c.extra != nil {
				c.extra(em)
			}
			_ = em.Final(ctx, c.final)
		})
		err := judge(c.adapter, evs)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
	if err := judge("native", nil); err == nil {
		t.Error("an empty trace passed")
	}
}

// writeFakeClaude writes a shell script that behaves like a minimal claude.
func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakeclaude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const okClaude = `echo '{"type":"system","subtype":"init"}'
while read l; do
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}}'
  echo '{"type":"result","subtype":"success","is_error":false}'
done
`

func runHarness(t *testing.T, args ...string) error {
	t.Helper()
	ev := filepath.Join(t.TempDir(), "events.jsonl")
	return harnessMain(append([]string{"run", "--events", ev}, args...))
}

func TestHarnessRunExitCodes(t *testing.T) {
	ok := writeFakeClaude(t, okClaude)
	if err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", ok, "--prompt", "hi", "--input", "again", "--cancel-input", "--wait", "1s"); err != nil {
		t.Fatalf("a healthy session failed: %v", err)
	}
	crash := writeFakeClaude(t, "exit 1\n")
	if err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", crash, "--prompt", "hi", "--wait", "1s"); err == nil {
		t.Fatal("a crashing child exited 0")
	}
	// answers once, reads the second input, never answers it, stays up until Stop
	lost := writeFakeClaude(t, `echo '{"type":"system","subtype":"init"}'
read l
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}'
echo '{"type":"result","subtype":"success","is_error":false}'
cat >/dev/null
`)
	if err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", lost, "--prompt", "hi", "--input", "SECOND", "--wait", "3s"); err == nil {
		t.Fatal("a live child that lost the second input exited 0")
	} else {
		t.Logf("lost: %v", err)
	}
	silent := writeFakeClaude(t, "cat >/dev/null\n")
	if err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", silent, "--prompt", "hi", "--wait", "1s"); err == nil {
		t.Fatal("a child that never answers exited 0")
	}
	errored := writeFakeClaude(t, `while read l; do echo '{"type":"result","subtype":"error_during_execution","is_error":true}'; done`+"\n")
	if err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", errored, "--prompt", "hi", "--wait", "1s"); err == nil {
		t.Fatal("a child whose turn failed exited 0")
	}
}

func TestHarnessRunRefusals(t *testing.T) {
	ok := writeFakeClaude(t, okClaude)
	for name, args := range map[string][]string{
		"no unconfined":     {"--adapter", "claude-code", "--claude-bin", ok, "--prompt", "hi"},
		"bad pass-env":      {"--adapter", "claude-code", "--unconfined", "--claude-bin", ok, "--prompt", "hi", "--pass-env", "FABRIC_DSN"},
		"native with env":   {"--adapter", "native", "--grant", "x", "--pass-env", "USER"},
		"unknown adapter":   {"--adapter", "codex"},
		"stray argument":    {"--adapter", "native", "--grant", "x", "extra"},
		"native no grant":   {"--adapter", "native"},
		"claude no prompt":  {"--adapter", "claude-code", "--unconfined"},
		"claude with grant": {"--adapter", "claude-code", "--unconfined", "--prompt", "p", "--grant", "g"},
	} {
		ev := filepath.Join(t.TempDir(), "events.jsonl")
		if err := harnessMain(append([]string{"run", "--events", ev}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(ev); err == nil {
			t.Errorf("%s: a usage error left a trace file behind", name)
		}
	}
	if err := harnessMain([]string{"run"}); err == nil {
		t.Errorf("missing --events: %v", err)
	}
}

// A credential passed to the child never reaches the trace or the printed final.
func TestPassedCredentialIsMaskedInTheTrace(t *testing.T) {
	const cred = "credential-canary-0123456789"
	t.Setenv("ANTHROPIC_API_KEY", cred)
	script := writeFakeClaude(t, `echo "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"key is $ANTHROPIC_API_KEY\"}]}}"
echo '{"type":"result","subtype":"success","is_error":false}'
cat >/dev/null
`)
	ev := filepath.Join(t.TempDir(), "events.jsonl")
	err := harnessMain([]string{"run", "--events", ev, "--adapter", "claude-code", "--unconfined", "--claude-bin", script,
		"--prompt", "hi", "--pass-env", "ANTHROPIC_API_KEY", "--wait", "1s"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ev)
	if strings.Contains(string(b), cred) {
		t.Fatal("the passed credential is in the trace")
	}
	if !strings.Contains(string(b), "[REDACTED]") {
		t.Fatal("the child never echoed the credential, so the test proves nothing")
	}
}

// A signal during the scripted session is a Stop, and the run must not read as
// success: a run cut short by `timeout` is not a completed test.
func TestHarnessRunSignalFailsTheRun(t *testing.T) {
	quiet := writeFakeClaude(t, "cat >/dev/null\n")
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	start := time.Now()
	err := runHarness(t, "--adapter", "claude-code", "--unconfined", "--claude-bin", quiet, "--prompt", "hi", "--wait", "60s")
	if err == nil {
		t.Fatal("a run cut short by a signal exited 0")
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("the signal did not stop the session promptly (%v)", d)
	}
}

func TestCodexPreflightRejectsModelAndAmbientFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--adapter", "codex"},
		{"--adapter", "codex", "--codex-preflight", "--codex-dir", "/owned", "--unconfined"},
		{"--adapter", "codex", "--codex-preflight", "--codex-dir", "/owned", "--pass-env", "USER"},
		{"--adapter", "codex", "--codex-preflight", "--codex-dir", "/owned", "--prompt", "history"},
		{"--adapter", "codex", "--codex-preflight", "--codex-dir", "/owned", "--wait", "121s"},
		{"--adapter", "native", "--grant", "x", "--codex-preflight"},
	} {
		ev := filepath.Join(t.TempDir(), "events.jsonl")
		if err := harnessMain(append([]string{"run", "--events", ev}, args...)); err == nil {
			t.Fatal("unsafe Codex flags accepted")
		}
		if _, err := os.Stat(ev); err == nil {
			t.Fatal("usage refusal created output")
		}
	}
}

func TestCodexArtifactRequiresExactOwnedPlainFile(t *testing.T) {
	dir := t.TempDir()
	if err := checkCodexArtifact(dir); err == nil {
		t.Fatal("missing artifact passed")
	}
	path := filepath.Join(dir, "demo.txt")
	if err := os.WriteFile(path, []byte("fabric\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkCodexArtifact(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkCodexArtifact(dir); err == nil {
		t.Fatal("executable artifact passed")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkCodexArtifact(dir); err == nil {
		t.Fatal("extra footprint passed")
	}
}
