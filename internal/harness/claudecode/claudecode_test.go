package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

const modeEnv = "FAKE_CLAUDE_MODE"

// secret is built at run time so no key-shaped literal sits in the source.
var secret = "sk-" + "ant-api03-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func TestMain(m *testing.M) {
	if mode := os.Getenv(modeEnv); mode != "" {
		fakeClaude(mode)
		return
	}
	os.Exit(m.Run())
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func assistant(blocks ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "session_id": "s1", "message": map[string]any{
		"role": "assistant", "content": blocks,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}}
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func result(sub string, isErr bool) map[string]any {
	return map[string]any{"type": "result", "subtype": sub, "is_error": isErr, "session_id": "s1", "num_turns": 1}
}

// fakeClaude plays the part of `claude --input-format stream-json` over stdio.
func fakeClaude(mode string) {
	if strings.HasPrefix(mode, "fixed") {
		fakeFixed(mode)
		return
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "s1", "model": "m"})
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	n := 0
	for sc.Scan() {
		var in struct {
			Type    string `json:"type"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(sc.Bytes(), &in) != nil {
			continue
		}
		if in.Type == "control_request" {
			emit(assistant(text("got-control:" + in.Request.Subtype)))
			continue
		}
		n++
		switch mode {
		case "normal", "unknown":
			emit(assistant(text("starting"), map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}))
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}}})
			if mode == "unknown" {
				emit(map[string]any{"type": "weirdo", "x": 1})
				fmt.Println("this is not json")
			}
			emit(assistant(text("all done")))
			emit(result("success", false))
		case "nofinal":
			emit(assistant(text("about to crash")))
			os.Exit(3)
		case "secret":
			emit(assistant(text("found " + os.Getenv("FAKE_SECRET") + " here")))
			emit(result("success", false))
		case "failed":
			emit(assistant(text("trying")))
			emit(result("error_during_execution", true))
		case "bigline":
			fmt.Println(`{"type":"assistant","message":{"content":"` + strings.Repeat("x", 5<<20) + `"}}`)
			emit(assistant(text("after the big line")))
			emit(result("success", false))
		case "env":
			emit(assistant(text(fmt.Sprintf("env_count=%d", len(os.Environ())))))
			emit(result("success", false))
		case "hooks", "silent", "toolerr", "argv", "pwd", "stderrsecret", "grandchild", "setsid":
			extraModes(mode)
		case "hang":
			time.Sleep(time.Hour) // ignores stdin close and never exits
		case "chatty":
			for { // keeps producing events until its stdout goes away
				emit(assistant(text("noise")))
				time.Sleep(2 * time.Millisecond)
			}
		case "block":
			if n == 1 {
				emit(assistant(text("working")))
			} else {
				emit(assistant(text("echo:" + in.Message.Content)))
			}
			emit(result("success", false))
		}
	}
}

func launcher(mode string, extra map[string]string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		cmd := exec.Command(os.Args[0])
		env := []string{modeEnv + "=" + mode}
		for k, v := range extra {
			env = append(env, k+"="+v)
		}
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		cmd.Dir = spec.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return startCmd(cmd)
	}
}

func adapter(mode string, single bool, extra map[string]string) harness.Adapter {
	return &Adapter{Launch: launcher(mode, extra), SingleTurn: single, StopGrace: 3 * time.Second}
}

func TestConformance(t *testing.T) {
	harnesstest.Conformance(t, harnesstest.Case{
		Normal:  func(harnesstest.T) harness.Adapter { return adapter("normal", true, nil) },
		Unknown: func(harnesstest.T) harness.Adapter { return adapter("unknown", true, nil) },
		NoFinal: func(harnesstest.T) harness.Adapter { return adapter("nofinal", true, nil) },
		Secret: func(harnesstest.T) (harness.Adapter, string) {
			return adapter("secret", true, map[string]string{"FAKE_SECRET": secret}), secret
		},
		Block: func(harnesstest.T) harness.Adapter { return adapter("block", false, nil) },
	})
}

func runToEnd(t *testing.T, a harness.Adapter, spec harness.StartSpec) (harness.Final, []harness.Event) {
	t.Helper()
	rec := &harnesstest.Recorder{}
	spec.SessionID = "ses_" + strings.ReplaceAll(t.Name(), "/", "_")
	spec.Dir = t.TempDir()
	s, err := a.Start(context.Background(), spec, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return f, rec.Snapshot()
}

func TestEnvironmentIsExactlyTheAllowList(t *testing.T) {
	t.Setenv("PARENT_ONLY_SENTINEL", "leak")
	f, evs := runToEnd(t, adapter("env", true, nil), harness.StartSpec{Prompt: "go", Env: map[string]string{"ALLOWED": "1"}})
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	// the fake sees modeEnv plus ALLOWED and nothing inherited
	var got string
	for _, x := range harnesstest.Texts(evs) {
		if strings.HasPrefix(x, "env_count=") {
			got = x
		}
	}
	if got != "env_count=2" {
		t.Fatalf("child environment is %q, want exactly the two allow-listed entries", got)
	}
}

func TestRefusesInheritedEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0]) // Env nil means inherit
	if _, err := startCmd(cmd); err == nil {
		t.Fatal("started a process with an inherited environment")
	}
	if envList(nil) == nil {
		t.Fatal("empty allow-list must be a non-nil empty list, not 'inherit'")
	}
}

func TestFailedTurnIsFailed(t *testing.T) {
	f, _ := runToEnd(t, adapter("failed", true, nil), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, "error_during_execution") {
		t.Fatalf("%+v", f)
	}
}

func TestOversizeLineBecomesUnknownAndSessionContinues(t *testing.T) {
	f, evs := runToEnd(t, adapter("bigline", true, nil), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if len(e.Payload) > 64<<10 {
			t.Fatalf("event %d is %d bytes: not bounded", e.Seq, len(e.Payload))
		}
		if e.Kind == harness.KindUnknown {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d unknown events, want 1 for the oversize line", n)
	}
	found := false
	for _, x := range harnesstest.Texts(evs) {
		found = found || x == "after the big line"
	}
	if !found {
		t.Fatal("the line after the oversize one was lost")
	}
}

func TestStopKillsAProcessThatIgnoresStdinClose(t *testing.T) {
	a := &Adapter{Launch: launcher("hang", nil), StopGrace: 300 * time.Millisecond}
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_hang", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeStopped {
		t.Fatalf("%+v", f)
	}
	if err := harnesstest.CheckStream(rec.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

func TestStartFailureAndRegistry(t *testing.T) {
	a := &Adapter{Launch: func(context.Context, harness.StartSpec) (Proc, error) { return nil, fmt.Errorf("no binary") }}
	if _, err := a.Start(context.Background(), harness.StartSpec{SessionID: "s"}, &harnesstest.Recorder{}); err == nil {
		t.Fatal("launch error swallowed")
	}
	if _, err := (&Adapter{}).Start(context.Background(), harness.StartSpec{SessionID: "s"}, &harnesstest.Recorder{}); err == nil {
		t.Fatal("missing launcher accepted")
	}
	r := harness.NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if c := a.Caps(); !c.Interactive {
		t.Fatalf("%+v", c)
	}
}

func TestReadLinesBounds(t *testing.T) {
	var got []string
	var over []bool
	readLines(strings.NewReader("ab\r\n"+strings.Repeat("z", 100)+"\nlast"), 10, func(l []byte, o bool) {
		got = append(got, string(l))
		over = append(over, o)
	})
	want := []string{"ab", strings.Repeat("z", 10), "last"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || over[0] || !over[1] || over[2] {
		t.Fatalf("got %q over %v", got, over)
	}
}

// Both an unrecognised event type and a line that is not JSON must surface as
// unknown events; neither may be dropped.
func TestUnknownTypeAndMalformedLineAreBothReported(t *testing.T) {
	_, evs := runToEnd(t, adapter("unknown", true, nil), harness.StartSpec{Prompt: "go"})
	n := 0
	for _, e := range evs {
		if e.Kind == harness.KindUnknown {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d unknown events, want 2 (one unrecognised type, one malformed line)", n)
	}
}

// A run whose events cannot be recorded must stop acting, not just report failure.
func TestSinkFailureEndsTheProcess(t *testing.T) {
	a := adapter("chatty", false, nil)
	rec := &harnesstest.Recorder{FailAfter: 4}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_chatty", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatalf("the process kept running after its events could not be recorded: %v", err)
	}
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("%+v", f)
	}
}
