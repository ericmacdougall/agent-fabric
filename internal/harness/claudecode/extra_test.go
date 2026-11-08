package claudecode

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
)

func init() {
	if os.Getenv(modeEnv) == "sleeper" {
		time.Sleep(6 * time.Second)
		os.Exit(0)
	}
}

// extraModes are fake-claude behaviours added after the PR 22 red-team.
func extraModes(mode string) {
	switch mode {
	case "silent":
		// reads stdin and never answers
	case "toolerr":
		emit(assistant(map[string]any{"type": "tool_use", "id": "toolu_e", "name": "Bash", "input": map[string]any{"command": "false"}}))
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_e", "is_error": true, "content": "boom"}}}})
		emit(result("success", false))
	case "argv":
		emit(assistant(text("argv=" + strings.Join(os.Args[1:], " "))))
		emit(result("success", false))
	case "pwd":
		wd, _ := os.Getwd()
		emit(assistant(text("pwd=" + wd)))
		emit(result("success", false))
	case "stderrsecret":
		// The secret comes first and the padding is sized so the 16 KiB tail
		// window starts a few bytes into it, losing the recognisable prefix.
		sec := os.Getenv("FAKE_SECRET")
		fmt.Fprintln(os.Stderr, sec+" "+strings.Repeat("x", 16384-len(sec)+9))
		os.Exit(1)
	case "grandchild", "setsid":
		c := exec.Command(os.Args[0])
		c.Env = []string{modeEnv + "=sleeper"}
		if mode == "setsid" {
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			c.Stdout = os.Stdout // holds the pipe open after the leader exits
		}
		_ = c.Start()
		emit(assistant(text(fmt.Sprintf("child=%d", c.Process.Pid))))
		emit(result("success", false))
		os.Exit(0)
	}
}

func execAdapter(mode string, env map[string]string) *Adapter {
	e := map[string]string{modeEnv: mode}
	for k, v := range env {
		e[k] = v
	}
	return &Adapter{Launch: func(ctx context.Context, spec harness.StartSpec) (Proc, error) {
		spec.Env = e
		return ExecLauncher(os.Args[0], "--fake")(ctx, spec)
	}, SingleTurn: true, StopGrace: 2 * time.Second, DrainGrace: 500 * time.Millisecond}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func childPID(t *testing.T, evs []harness.Event) int {
	t.Helper()
	for _, x := range harnesstest.Texts(evs) {
		var n int
		if _, err := fmt.Sscanf(x, "child=%d", &n); err == nil {
			return n
		}
	}
	t.Fatal("no child pid reported")
	return 0
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// The shipped launcher, not a test stand-in, is what the security claims hold for.
func TestExecLauncherEnvironmentArgvAndDir(t *testing.T) {
	t.Setenv("PARENT_ONLY_SENTINEL", "leak")
	f, evs := runToEnd(t, execAdapter("env", nil), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeDone || !contains(harnesstest.Texts(evs), "env_count=1") {
		t.Fatalf("env through ExecLauncher: %+v %v", f, harnesstest.Texts(evs))
	}
	_, evs = runToEnd(t, execAdapter("argv", nil), harness.StartSpec{Prompt: "go", Model: "claude-x"})
	if !contains(harnesstest.Texts(evs), "argv=--print --input-format stream-json --output-format stream-json --verbose --model=claude-x --fake") {
		t.Fatalf("argv: %v", harnesstest.Texts(evs))
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "a b", "x;y", "-x"} {
		a := execAdapter("argv", nil)
		if _, err := a.Start(context.Background(), harness.StartSpec{SessionID: "s", Prompt: "go", Dir: t.TempDir(), Model: bad}, &harnesstest.Recorder{}); err == nil {
			t.Errorf("model %q accepted", bad)
		}
	}
	for _, dir := range []string{"", "relative/dir"} {
		a := execAdapter("pwd", nil)
		if _, err := a.Start(context.Background(), harness.StartSpec{SessionID: "s", Prompt: "go", Dir: dir}, &harnesstest.Recorder{}); err == nil {
			t.Errorf("dir %q accepted", dir)
		}
	}
}

// Interrupt must reach the child as a control request.
func TestInterruptReachesTheChild(t *testing.T) {
	a := adapter("block", false, nil)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_int", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return contains(harnesstest.Texts(e), "got-control:interrupt") }) {
		t.Fatal("the child never received the interrupt control request")
	}
	_ = s.Stop(ctx)
}

// A descendant left in the process group dies with the session.
func TestNaturalExitKillsTheProcessGroup(t *testing.T) {
	f, evs := runToEnd(t, execAdapter("grandchild", nil), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	pid := childPID(t, evs)
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("descendant %d survived the session", pid)
	}
}

// A descendant that left the group and holds stdout cannot hold the session.
func TestStdoutHolderOutsideTheGroupDoesNotBlockSettle(t *testing.T) {
	start := time.Now()
	f, _ := runToEnd(t, execAdapter("setsid", nil), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("%+v", f)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("settled after %v: the stdout holder blocked the session", d)
	}
}

// Input and Interrupt honour their context when the child has stopped reading.
func TestInputHonoursContextWhenChildStopsReading(t *testing.T) {
	a := adapter("silent", false, nil)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_bp", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 1<<20)
	done := make(chan error, 1)
	go func() {
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		for i := 0; i < 400 && err == nil; i++ {
			err = s.Input(ctx, big)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("400 MiB of input never blocked or failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Input ignored its context")
	}
	ictx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_ = s.Interrupt(ictx)
	if time.Since(t0) > 3*time.Second {
		t.Fatal("Interrupt ignored its context")
	}
	sctx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	if err := s.Stop(sctx); err != nil {
		t.Fatal(err)
	}
}

// Undelivered inputs are counted deterministically.
func TestUndeliveredInputsAreReported(t *testing.T) {
	a := adapter("silent", false, nil)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_ud", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Input(ctx, "one")
	_ = s.Input(ctx, "two")
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeStopped || f.Reason != "undelivered_inputs=3" {
		t.Fatalf("%+v", f)
	}
}

func TestToolErrorIsReportedAsError(t *testing.T) {
	_, evs := runToEnd(t, adapter("toolerr", true, nil), harness.StartSpec{Prompt: "go"})
	var statuses []string
	for _, e := range evs {
		if e.Kind == harness.KindToolCall {
			var tc harness.ToolCall
			_ = json.Unmarshal(e.Payload, &tc)
			statuses = append(statuses, tc.Status)
		}
	}
	if len(statuses) != 2 || statuses[0] != "requested" || statuses[1] != "error" {
		t.Fatalf("tool call statuses %v", statuses)
	}
}

// A key cut in half by the stderr tail window must not leak its remainder.
func TestStderrTailDoesNotLeakPartialSecret(t *testing.T) {
	f, _ := runToEnd(t, execAdapter("stderrsecret", map[string]string{"FAKE_SECRET": secret}), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("%+v", f)
	}
	for i := 0; i+8 <= len(secret); i++ {
		if strings.Contains(f.Reason, secret[i:i+8]) {
			t.Fatalf("reason carries part of the secret: %q", secret[i:i+8])
		}
	}
}
