package kimi

import (
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
	"fabric/internal/harness/proc"
)

const modeEnv = "FAKE_KIMI_MODE"

// secret is built at run time so no key-shaped literal sits in the source.
var secret = "sk-" + "ant-api03-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func line(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func meta() {
	line(map[string]any{"role": "meta", "type": "system.version", "version": "0"})
}

func resume() {
	line(map[string]any{"role": "meta", "type": "session.resume_hint", "session_id": "session_x", "content": "To resume"})
}

func init() {
	mode := os.Getenv(modeEnv)
	if mode == "" {
		return
	}
	meta()
	switch mode {
	case "normal", "unknown":
		line(map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "call_1", "function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`}}}})
		line(map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "shell", "content": "a b"})
		if mode == "unknown" {
			line(map[string]any{"role": "weird", "x": 1})
			fmt.Println("not json")
		}
		line(map[string]any{"role": "assistant", "content": "all done"})
		resume()
	case "nofinal":
		line(map[string]any{"role": "assistant", "content": "about to crash"})
		os.Exit(3)
	case "empty":
		resume()
	case "hookonly":
		// a hook result is assistant-shaped but is not a model answer
		line(map[string]any{"role": "assistant", "content": "Stop hook\n\nran a hook"})
		os.Exit(0)
	case "slowdrain":
		for i := 0; i < 420; i++ { // ~110 KB: fits in the pipe plus the read buffer, so the child exits while the sink is still working
			line(map[string]any{"role": "assistant", "content": strings.Repeat("p", 240)})
		}
		line(map[string]any{"role": "assistant", "content": "last-line"})
		os.Exit(0)
	case "secret":
		line(map[string]any{"role": "assistant", "content": "found " + os.Getenv("FAKE_SECRET") + " here"})
		line(map[string]any{"role": "assistant", "content": "x"})
	case "block":
		line(map[string]any{"role": "assistant", "content": "working"})
		time.Sleep(time.Hour)
	case "env":
		line(map[string]any{"role": "assistant", "content": fmt.Sprintf("env_count=%d", len(os.Environ()))})
	case "argv":
		line(map[string]any{"role": "assistant", "content": "argv=" + strings.Join(os.Args[1:], "|")})
	case "stderrsecret":
		sec := os.Getenv("FAKE_SECRET")
		fmt.Fprintln(os.Stderr, sec+" "+strings.Repeat("x", 16384-len(sec)+9))
		os.Exit(1)
	case "setsid":
		c := exec.Command(os.Args[0])
		c.Env = []string{modeEnv + "=sleeper"}
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		c.Stdout = os.Stdout // holds stdout open after the leader exits
		_ = c.Start()
		line(map[string]any{"role": "assistant", "content": "done"})
	case "sleeper":
		time.Sleep(6 * time.Second)
	}
	os.Exit(0)
}

func exec2(mode string, env map[string]string) *Adapter {
	e := map[string]string{modeEnv: mode}
	for k, v := range env {
		e[k] = v
	}
	return &Adapter{DrainGrace: 400 * time.Millisecond, Launch: func(ctx context.Context, spec harness.StartSpec) (proc.Proc, error) {
		spec.Env = e
		return ExecLauncher(os.Args[0], "--fake")(ctx, spec)
	}}
}

func runToEnd(t *testing.T, a *Adapter, spec harness.StartSpec) (harness.Final, []harness.Event) {
	t.Helper()
	rec := &harnesstest.Recorder{}
	spec.SessionID = "ses_" + strings.ReplaceAll(t.Name(), "/", "_")
	spec.Dir = t.TempDir()
	if spec.Prompt == "" {
		spec.Prompt = "go"
	}
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

func TestConformance(t *testing.T) {
	mk := func(mode string) func(harnesstest.T) harness.Adapter {
		return func(harnesstest.T) harness.Adapter { return exec2(mode, nil) }
	}
	harnesstest.Conformance(t, harnesstest.Case{
		Normal: mk("normal"), Unknown: mk("unknown"), NoFinal: mk("nofinal"), Block: mk("block"),
		Secret: func(harnesstest.T) (harness.Adapter, string) {
			return exec2("secret", map[string]string{"FAKE_SECRET": secret}), secret
		},
		Optional: []harness.Kind{harness.KindUsage}, // the CLI reports no usage
	})
}

func TestUnverifiedLabel(t *testing.T) {
	f, evs := runToEnd(t, exec2("normal", nil), harness.StartSpec{})
	if f.Outcome != harness.OutcomeDone || f.Reason != UnverifiedNotice {
		t.Fatalf("a done run must still carry the unverified label: %+v", f)
	}
	first := harnesstest.Texts(evs)
	if len(first) == 0 || first[0] != UnverifiedNotice {
		t.Fatalf("the stream does not start with the unverified notice: %v", first)
	}
}

func TestExitZeroWithoutReplyIsFailed(t *testing.T) {
	f, _ := runToEnd(t, exec2("empty", nil), harness.StartSpec{})
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("%+v", f)
	}
}

func TestLauncherEnvArgvAndRefusals(t *testing.T) {
	t.Setenv("PARENT_ONLY_SENTINEL", "leak")
	_, evs := runToEnd(t, exec2("env", nil), harness.StartSpec{})
	if !contains(harnesstest.Texts(evs), "env_count=1") {
		t.Fatalf("env: %v", harnesstest.Texts(evs))
	}
	_, evs = runToEnd(t, exec2("argv", nil), harness.StartSpec{Prompt: "-x --yolo", Model: "k3"})
	if !contains(harnesstest.Texts(evs), "argv=--prompt=-x --yolo|--output-format|stream-json|--model=k3|--fake") {
		t.Fatalf("argv: %v", harnesstest.Texts(evs))
	}
	for name, spec := range map[string]harness.StartSpec{
		"relative dir": {SessionID: "s", Prompt: "p", Dir: "rel"},
		"empty dir":    {SessionID: "s", Prompt: "p"},
		"empty prompt": {SessionID: "s", Dir: t.TempDir()},
		"big prompt":   {SessionID: "s", Prompt: strings.Repeat("a", MaxPromptBytes+1), Dir: t.TempDir()},
		"flag model":   {SessionID: "s", Prompt: "p", Dir: t.TempDir(), Model: "--auto"},
		"spaced model": {SessionID: "s", Prompt: "p", Dir: t.TempDir(), Model: "a b"},
	} {
		if _, err := exec2("env", nil).Start(context.Background(), spec, &harnesstest.Recorder{}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestStderrCutInsideSecretDoesNotLeak(t *testing.T) {
	f, _ := runToEnd(t, exec2("stderrsecret", map[string]string{"FAKE_SECRET": secret}), harness.StartSpec{})
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("%+v", f)
	}
	for i := 0; i+8 <= len(secret); i++ {
		if strings.Contains(f.Reason, secret[i:i+8]) {
			t.Fatalf("reason carries part of the secret: %q", secret[i:i+8])
		}
	}
}

func TestStdoutHolderOutsideTheGroupDoesNotBlockSettle(t *testing.T) {
	start := time.Now()
	f, _ := runToEnd(t, exec2("setsid", nil), harness.StartSpec{})
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("settled after %v: the stdout holder blocked the session", d)
	}
}

func TestRegistry(t *testing.T) {
	r := harness.NewRegistry()
	if err := r.Register(&Adapter{}); err != nil {
		t.Fatal(err)
	}
	if (&Adapter{}).Caps().Interactive {
		t.Fatal("kimi is one-shot")
	}
	if _, err := (&Adapter{}).Start(context.Background(), harness.StartSpec{SessionID: "s"}, &harnesstest.Recorder{}); err == nil {
		t.Fatal("missing launcher accepted")
	}
}

// A hook result is not a model answer: exit 0 with only hook output is failed.
func TestHookOutputIsNotAReply(t *testing.T) {
	f, evs := runToEnd(t, exec2("hookonly", nil), harness.StartSpec{})
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("hook output alone must not make a done run: %+v", f)
	}
	for _, e := range evs {
		if e.Kind == harness.KindMessage && strings.Contains(string(e.Payload), `"role":"assistant"`) {
			t.Fatalf("hook output recorded as an assistant message: %s", e.Payload)
		}
	}
}

// The prompt travels in argv, so a secret-shaped prompt is refused before launch.
func TestSecretShapedPromptIsRefused(t *testing.T) {
	_, err := exec2("env", nil).Start(context.Background(), harness.StartSpec{SessionID: "s", Dir: t.TempDir(), Prompt: "use " + secret + " now"}, &harnesstest.Recorder{})
	if err == nil {
		t.Fatal("a prompt holding a secret-shaped string was accepted")
	}
}

// Checks live in Start, so a custom launcher cannot bypass them.
func TestStartChecksApplyToAnyLauncher(t *testing.T) {
	launched := false
	a := &Adapter{Launch: func(context.Context, harness.StartSpec) (proc.Proc, error) { launched = true; return nil, nil }}
	for _, spec := range []harness.StartSpec{
		{SessionID: "s", Prompt: "p", Dir: "rel"},
		{SessionID: "s", Prompt: "", Dir: "/tmp"},
		{SessionID: "s", Prompt: "p", Dir: "/tmp", Model: "--auto"},
	} {
		if _, err := a.Start(context.Background(), spec, &harnesstest.Recorder{}); err == nil {
			t.Errorf("spec %+v accepted", spec)
		}
	}
	if launched {
		t.Fatal("the launcher ran for an invalid spec")
	}
}

// A slow sink must not turn a finished run into a failure (PR 22 class D2).
type slowSink struct {
	harnesstest.Recorder
	delay time.Duration
}

func (s *slowSink) Emit(ctx context.Context, e harness.Event) error {
	time.Sleep(s.delay)
	return s.Recorder.Emit(ctx, e)
}

func TestSlowSinkDoesNotLoseTheLastReply(t *testing.T) {
	a := exec2("slowdrain", nil)
	a.DrainGrace = 100 * time.Millisecond
	sink := &slowSink{delay: 5 * time.Millisecond}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_slow", Prompt: "go", Dir: t.TempDir()}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeDone || !contains(harnesstest.Texts(sink.Snapshot()), "last-line") {
		t.Fatalf("a slow sink lost output: %+v", f)
	}
}

// The label survives stop and interrupt too.
func TestLabelOnStopAndInterrupt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		rec := &harnesstest.Recorder{}
		s, err := exec2("block", nil).Start(context.Background(), harness.StartSpec{SessionID: "ses_lbl", Prompt: "go", Dir: t.TempDir()}, rec)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return len(e) >= 4 }) {
			t.Fatal("no output")
		}
		if interrupt {
			_ = s.Interrupt(ctx)
		} else {
			_ = s.Stop(ctx)
		}
		f, _ := s.Wait(ctx)
		cancel()
		if f.Reason != UnverifiedNotice {
			t.Fatalf("interrupt=%v: final %+v lacks the unverified label", interrupt, f)
		}
	}
}
