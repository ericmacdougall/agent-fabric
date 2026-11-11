package codex_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--fabric-test-script" {
		runFake(os.Args[2])
		return
	}
	if p := os.Getenv("FAKE_CODEX_SCRIPT"); p != "" {
		runFake(p)
		return
	}
	os.Exit(m.Run())
}

// canary is built at run time so no key-shaped literal sits in the source.
var canary = "sk-" + "ant-api03-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func exe(t tb) string {
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func script(t tb, name string) string {
	p, err := filepath.Abs(filepath.Join("testdata", name+".script"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func adapter(t tb, name string, env ...string) *codex.Adapter {
	base := map[string]string{"FAKE_CODEX_SCRIPT": script(t, name)}
	for i := 0; i+1 < len(env); i += 2 {
		base[env[i]] = env[i+1]
	}
	return &codex.Adapter{Command: []string{exe(t)}, BaseEnv: base, KillGrace: 300 * time.Millisecond, RequestTimeout: 5 * time.Second}
}

func startSpec(t testing.TB, prompt string) harness.StartSpec {
	return harness.StartSpec{SessionID: "ses_" + strings.ReplaceAll(t.Name(), "/", "_"), Prompt: prompt, Dir: t.TempDir()}
}

func begin(t testing.TB, a harness.Adapter, spec harness.StartSpec) (harness.Session, *harnesstest.Recorder) {
	t.Helper()
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), spec, rec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s, rec
}

func finish(t testing.TB, s harness.Session) harness.Final {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return f
}

// runOnce runs a session to its end.
func runOnce(t testing.TB, a harness.Adapter, spec harness.StartSpec) ([]harness.Event, harness.Final) {
	t.Helper()
	s, rec := begin(t, a, spec)
	f := finish(t, s)
	evs := rec.Snapshot()
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	return evs, f
}

func decode[T any](t testing.TB, e harness.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ofKind(evs []harness.Event, k harness.Kind) []harness.Event {
	var out []harness.Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func hasText(evs []harness.Event, sub string) bool {
	for _, x := range harnesstest.Texts(evs) {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// noSessionGoroutines fails if a codex session goroutine outlives its Final.
func noSessionGoroutines(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var dump string
	for time.Now().Before(deadline) {
		buf := make([]byte, 1<<20)
		dump = string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(dump, "harness/codex.(*session)") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines of a finished session are still running:\n%s", dump)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pidFrom(t testing.TB, evs []harness.Event) int {
	t.Helper()
	for _, x := range harnesstest.Texts(evs) {
		if strings.HasPrefix(x, "pid:") {
			n, err := strconv.Atoi(strings.TrimPrefix(x, "pid:"))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("the fake did not report its pid")
	return 0
}

// ---- conformance ----

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Normal:  func(t harnesstest.T) harness.Adapter { return adapter(t, "normal") },
		Unknown: func(t harnesstest.T) harness.Adapter { return adapter(t, "unknown") },
		NoFinal: func(t harnesstest.T) harness.Adapter { return adapter(t, "nofinal") },
		Secret: func(t harnesstest.T) (harness.Adapter, string) {
			return adapter(t, "secret", "FAKE_SECRET", canary), canary
		},
		Block: func(t harnesstest.T) harness.Adapter {
			a := adapter(t, "block")
			a.KeepOpen = true
			return a
		},
	}
}

func TestConformance(t *testing.T) {
	harnesstest.Conformance(t, conformanceCase())
	noSessionGoroutines(t)
}

func TestRegisters(t *testing.T) {
	r := harness.NewRegistry()
	a := &codex.Adapter{}
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("codex")
	if !ok || got != harness.Adapter(a) {
		t.Fatal("codex adapter is not retrievable by name")
	}
	if c := a.Caps(); !c.Interactive || !c.Resume || c.Subagents {
		t.Fatalf("caps %+v", c)
	}
}

// ---- event mapping ----

func TestNormalMapping(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "normal"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	// user message, turn, tool requested, tool ok, assistant message, usage, final
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind))
	}
	want := "message,turn,tool_call,tool_call,message,usage,final"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("kinds %s, want %s", got, want)
	}
	tools := ofKind(evs, harness.KindToolCall)
	sum := sha256.Sum256([]byte("echo hi"))
	for i, st := range []string{"requested", "ok"} {
		tc := decode[harness.ToolCall](t, tools[i])
		if tc.ID != "cmd_1" || tc.Name != "command" || tc.Status != st || tc.InputSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("tool call %d: %+v", i, tc)
		}
		if strings.Contains(string(tools[i].Payload), "echo hi") {
			t.Fatal("the tool input leaked into the event")
		}
	}
	u := decode[harness.Usage](t, ofKind(evs, harness.KindUsage)[0])
	if u != (harness.Usage{Input: 120, Output: 30, CacheCreate: 5, CacheRead: 20}) {
		t.Fatalf("usage %+v", u)
	}
	if !hasText(evs, "hello") {
		t.Fatal("assistant message missing")
	}
	noSessionGoroutines(t)
}

func TestWarningsAreMessagesAndChatterIsNotUnknown(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "warning"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	for _, want := range []string{"codex warning: careful now", "codex warning: guard says hi"} {
		if !hasText(evs, want) {
			t.Errorf("missing %q in %q", want, harnesstest.Texts(evs))
		}
	}
	if hasText(evs, "somebody else") {
		t.Error("a warning for another thread was reported as this session's")
	}
	// Only the other thread's warning is unclassified; readState chatter is not.
	if n := len(ofKind(evs, harness.KindUnknown)); n != 1 {
		t.Errorf("%d unknown events, want 1", n)
	}
}

func TestFramesBeforeTheReplyAreBound(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "early"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind))
	}
	// The turn is bound by the reply, so Turn precedes the buffered frames.
	want := "message,turn,tool_call,tool_call,message,usage,final"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("kinds %s, want %s", got, want)
	}
	if !hasText(evs, "early") {
		t.Fatal("buffered message lost")
	}
}

func TestTurnParameters(t *testing.T) {
	a := adapter(t, "resume")
	spec := startSpec(t, "continue it")
	spec.Extra = json.RawMessage(`{"resume_thread":"thr_old"}`)
	spec.Model = "gpt-test"
	s, rec := begin(t, a, spec)
	f := finish(t, s)
	evs := rec.Snapshot()
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	if id := s.(interface{ ThreadID() string }).ThreadID(); id != "thr_old" {
		t.Fatalf("thread %q", id)
	}
	var params string
	for _, x := range harnesstest.Texts(evs) {
		if strings.HasPrefix(x, "resume:") {
			params = strings.TrimPrefix(x, "resume:")
		}
	}
	var p struct {
		ThreadID       string `json:"threadId"`
		Cwd            string `json:"cwd"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Model          string `json:"model"`
		Input          []struct{ Type, Text string }
		SandboxPolicy  struct {
			Type          string   `json:"type"`
			WritableRoots []string `json:"writableRoots"`
			NetworkAccess bool     `json:"networkAccess"`
		} `json:"sandboxPolicy"`
	}
	if err := json.Unmarshal([]byte(params), &p); err != nil {
		t.Fatalf("turn/start params %q: %v", params, err)
	}
	if p.ThreadID != "thr_old" || p.Cwd != spec.Dir || p.ApprovalPolicy != "never" || p.Model != "gpt-test" ||
		len(p.Input) != 1 || p.Input[0].Text != "continue it" ||
		p.SandboxPolicy.Type != "workspaceWrite" || p.SandboxPolicy.NetworkAccess ||
		len(p.SandboxPolicy.WritableRoots) != 1 || p.SandboxPolicy.WritableRoots[0] != spec.Dir {
		t.Fatalf("unexpected turn/start params: %+v", p)
	}
}

func TestResumeMustReturnTheRequestedThread(t *testing.T) {
	spec := startSpec(t, "x")
	spec.Extra = json.RawMessage(`{"resume_thread":"thr_old"}`)
	_, f := runOnce(t, adapter(t, "resume_wrong"), spec)
	if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, "different thread") {
		t.Fatalf("final %+v", f)
	}
}

func TestFailedTurn(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "failed"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, "model exploded") {
		t.Fatalf("final %+v", f)
	}
	if !hasText(evs, "codex error: model exploded") {
		t.Fatal("the error notification was not reported")
	}
}

func TestRejectedAndMalformedBindings(t *testing.T) {
	for name, want := range map[string]string{
		"rejected":      "no such model",
		"dup_binding":   "turn",
		"reply_no_turn": "turn",
	} {
		_, f := runOnce(t, adapter(t, name), startSpec(t, "go"))
		if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, want) {
			t.Errorf("%s: final %+v", name, f)
		}
	}
}

func TestUnansweredRequestTimesOut(t *testing.T) {
	a := adapter(t, "noinit")
	a.RequestTimeout = 200 * time.Millisecond
	_, f := runOnce(t, a, startSpec(t, "go"))
	if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, "did not answer initialize") {
		t.Fatalf("final %+v", f)
	}
	noSessionGoroutines(t)
}

func TestExitWithoutResult(t *testing.T) {
	_, f := runOnce(t, adapter(t, "nofinal"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeFailed || f.ExitCode != 3 || !strings.Contains(f.Reason, "without a result") {
		t.Fatalf("final %+v", f)
	}
	_, f = runOnce(t, adapter(t, "exit0"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeFailed || f.ExitCode != 0 || !strings.Contains(f.Reason, "without a result") {
		t.Fatalf("a clean exit without a result must still be failed: %+v", f)
	}
	noSessionGoroutines(t)
}

// ---- hostile protocol input ----

func TestUnknownIsData(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "unknown"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	var whys []string
	for _, e := range ofKind(evs, harness.KindUnknown) {
		whys = append(whys, decode[harness.Unknown](t, e).Why)
	}
	for _, want := range []string{"unrecognised notification mystery/event", "malformed JSON", "did not send", "item type holographicThing", "not a request, response or notification"} {
		found := false
		for _, w := range whys {
			if strings.Contains(w, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no unknown event mentions %q; got %q", want, whys)
		}
	}
}

func TestIdentityRules(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "identity"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	for _, bad := range []string{"foreign turn", "foreign thread", "no turn id", "duplicate keys", "replayed id"} {
		if hasText(evs, bad) {
			t.Errorf("message %q was accepted", bad)
		}
	}
	if !hasText(evs, "genuine") {
		t.Error("the genuine message is missing")
	}
	if n := len(ofKind(evs, harness.KindUnknown)); n != 7 {
		t.Errorf("%d unknown events, want 7 (one per rejected frame)", n)
	}
}

func TestOversizeLineIsUnknownAndStreamContinues(t *testing.T) {
	a := adapter(t, "oversize")
	a.MaxLine = 4096
	evs, f := runOnce(t, a, startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone || !hasText(evs, "after") {
		t.Fatalf("final %+v", f)
	}
	found := false
	for _, e := range ofKind(evs, harness.KindUnknown) {
		u := decode[harness.Unknown](t, e)
		if strings.Contains(u.Why, "exceeds the 4096 byte limit") {
			found = true
			if u.Raw != "" {
				t.Error("an oversize line must not be echoed")
			}
		}
	}
	if !found {
		t.Fatal("no unknown event for the oversize line")
	}
}

func TestTotalOutputIsBounded(t *testing.T) {
	a := adapter(t, "normal")
	a.MaxTotal = 800
	_, f := runOnce(t, a, startSpec(t, "go"))
	if f.Outcome != harness.OutcomeFailed || !strings.Contains(f.Reason, "output exceeded 800 bytes") {
		t.Fatalf("final %+v", f)
	}
	noSessionGoroutines(t)
}

func TestUnknownEventsAreCapped(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "flood"), startSpec(t, "go"))
	if n := len(ofKind(evs, harness.KindUnknown)); n != 256 {
		t.Fatalf("%d unknown events, want the cap of 256", n)
	}
	if f.Outcome != harness.OutcomeDone || !strings.Contains(f.Reason, "unknown_events_suppressed=44") {
		t.Fatalf("the suppressed count must be reported: %+v", f)
	}
}

func TestApprovalsAreDeclinedNeverGranted(t *testing.T) {
	evs, f := runOnce(t, adapter(t, "approval"), startSpec(t, "go"))
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	var denied *harness.ToolCall
	for _, e := range ofKind(evs, harness.KindToolCall) {
		tc := decode[harness.ToolCall](t, e)
		if tc.ID == "cmd_9" {
			denied = &tc
		}
	}
	if denied == nil || denied.Status != "denied" || denied.Name != "command" {
		t.Fatalf("approval request not reported as a denied tool call: %+v", denied)
	}
	var r1, r2 string
	for _, x := range harnesstest.Texts(evs) {
		if strings.HasPrefix(x, "resp1:") {
			r1 = x
		}
		if strings.HasPrefix(x, "resp2:") {
			r2 = x
		}
	}
	if !strings.Contains(r1, `"decision":"decline"`) || strings.Contains(r1, "accept") {
		t.Errorf("approval reply: %s", r1)
	}
	if !strings.Contains(r2, `"error"`) || strings.Contains(r2, `"result"`) {
		t.Errorf("an unsupported server request must get an error reply: %s", r2)
	}
}

// ---- process hygiene ----

func TestEnvironmentIsAllowListed(t *testing.T) {
	t.Setenv("FABRIC_LEAK_CANARY", "leaked")
	a := adapter(t, "envdump", "FROM_ADAPTER", "1")
	spec := startSpec(t, "the prompt must not reach argv")
	spec.Env = map[string]string{"FROM_SPEC": "1"}
	evs, f := runOnce(t, a, spec)
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	var env, args string
	for _, x := range harnesstest.Texts(evs) {
		if strings.HasPrefix(x, "env:") {
			env = strings.TrimPrefix(x, "env:")
		}
		if strings.HasPrefix(x, "args:") {
			args = strings.TrimPrefix(x, "args:")
		}
	}
	got := map[string]bool{}
	for _, k := range strings.Split(env, ",") {
		got[k] = true
	}
	for _, k := range []string{"FAKE_CODEX_SCRIPT", "FROM_ADAPTER", "FROM_SPEC"} {
		if !got[k] {
			t.Errorf("allow-listed variable %s missing from %q", k, env)
		}
	}
	for k := range got {
		switch k {
		case "FAKE_CODEX_SCRIPT", "FROM_ADAPTER", "FROM_SPEC":
		default:
			if !strings.HasPrefix(k, "GOCOVER") { // coverage runs add their own
				t.Errorf("variable %s reached the process without being allow-listed", k)
			}
		}
	}
	if args != "" {
		t.Errorf("argv carries %q; nothing from the spec may be passed in argv", args)
	}
}

func TestStopEndsTheProcessAndIsIdempotent(t *testing.T) {
	s, rec := begin(t, adapter(t, "hang_pid"), startSpec(t, "go"))
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "pid:") }) {
		t.Fatal("process did not report")
	}
	pid := pidFrom(t, rec.Snapshot())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := s.Stop(ctx); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
	f := finish(t, s)
	if f.Outcome != harness.OutcomeStopped {
		t.Fatalf("final %+v", f)
	}
	if alive(pid) {
		t.Fatal("the codex process survived Stop")
	}
	if err := s.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt after the end: %v", err)
	}
	if err := s.Input(ctx, "late"); !errors.Is(err, harness.ErrSessionEnded) {
		t.Fatalf("Input after the end: %v", err)
	}
	evs := rec.Snapshot()
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	noSessionGoroutines(t)
}

func TestProcessIgnoringSIGTERMIsKilled(t *testing.T) {
	s, rec := begin(t, adapter(t, "term_ignored"), startSpec(t, "go"))
	f := finish(t, s)
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	pid := pidFrom(t, rec.Snapshot())
	if alive(pid) {
		t.Fatal("a process that ignores SIGTERM survived the end of the session")
	}
	noSessionGoroutines(t)
}

// ---- interrupt and input ----

func TestInterruptCancelsTheTurnAndKeepsTheSession(t *testing.T) {
	s, rec := begin(t, adapter(t, "interrupt"), startSpec(t, "go"))
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "working") }) {
		t.Fatal("turn did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := s.Interrupt(ctx); err != nil {
			t.Fatalf("Interrupt %d: %v", i, err)
		}
	}
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "turn interrupted") }) {
		t.Fatal("the interrupt was not carried out")
	}
	short, c2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer c2()
	if _, err := s.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an interrupted session must stay open, Wait returned %v", err)
	}
	if err := s.Input(ctx, "next"); err != nil {
		t.Fatalf("Input after Interrupt: %v", err)
	}
	f := finish(t, s)
	evs := rec.Snapshot()
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
	if !hasText(evs, "echo:next") {
		t.Fatal("the input after the interrupt was not answered")
	}
	if !hasText(evs, "interrupts:1") || hasText(evs, "interrupts:2") {
		t.Fatalf("turn/interrupt must be sent exactly once: %q", harnesstest.Texts(evs))
	}
	// A second turn/interrupt would be answered by the fake after the turn
	// ended, and that late reply shows up as an unclassifiable event.
	if n := len(ofKind(evs, harness.KindUnknown)); n != 0 {
		t.Fatalf("%d unknown events after a repeated Interrupt; turn/interrupt was sent more than once", n)
	}
	noSessionGoroutines(t)
}

func TestInterruptBeforeTheTurnIsBound(t *testing.T) {
	s, rec := begin(t, adapter(t, "interrupt_early"), startSpec(t, "go"))
	// The user message is emitted when the turn is dispatched; the fake holds
	// the turn/start reply for 150ms, so the turn exists but is not bound.
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return len(e) >= 1 }) {
		t.Fatal("no dispatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "turn interrupted") }) {
		t.Fatal("an interrupt requested before binding was lost")
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if f := finish(t, s); f.Outcome != harness.OutcomeStopped {
		t.Fatalf("final %+v", f)
	}
}

func TestStopReportsUndeliveredInputs(t *testing.T) {
	a := adapter(t, "hang_pid")
	a.KeepOpen = true
	s, rec := begin(t, a, startSpec(t, "go"))
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return hasText(e, "pid:") }) {
		t.Fatal("process did not report")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The turn never completes, so these stay queued behind it.
	for _, m := range []string{"a", "b", "c"} {
		if err := s.Input(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	f := finish(t, s)
	if f.Outcome != harness.OutcomeStopped || !strings.Contains(f.Reason, "undelivered_inputs=3") {
		t.Fatalf("final %+v", f)
	}
}

func TestInputIsRefusedWhenTooLargeOrQueueFull(t *testing.T) {
	a := adapter(t, "hang_pid")
	a.KeepOpen = true
	s, _ := begin(t, a, startSpec(t, "go"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Input(ctx, strings.Repeat("x", codex.MaxInputBytes+1)); err == nil {
		t.Fatal("an oversize input was accepted")
	}
	var err error
	for i := 0; i <= codex.MaxQueuedInputs && err == nil; i++ {
		err = s.Input(ctx, "q")
	}
	if err == nil || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("the input queue is unbounded: %v", err)
	}
}

// ---- configuration ----

func TestStartValidation(t *testing.T) {
	good := startSpec(t, "x")
	cases := map[string]func(*harness.StartSpec){
		"unknown extra field": func(s *harness.StartSpec) { s.Extra = json.RawMessage(`{"nope":1}`) },
		"full access":         func(s *harness.StartSpec) { s.Extra = json.RawMessage(`{"sandbox":"danger-full-access"}`) },
		"bad approval":        func(s *harness.StartSpec) { s.Extra = json.RawMessage(`{"approval_policy":"always"}`) },
		"no dir":              func(s *harness.StartSpec) { s.Dir = "" },
		"empty prompt":        func(s *harness.StartSpec) { s.Prompt = "" },
		"bad env name":        func(s *harness.StartSpec) { s.Env = map[string]string{"A=B": "1"} },
		"nul in env":          func(s *harness.StartSpec) { s.Env = map[string]string{"A": "1\x00"} },
		"huge prompt":         func(s *harness.StartSpec) { s.Prompt = strings.Repeat("x", codex.MaxInputBytes+1) },
	}
	for name, mut := range cases {
		spec := good
		mut(&spec)
		if s, err := adapter(t, "normal").Start(context.Background(), spec, &harnesstest.Recorder{}); err == nil {
			_ = s.Stop(context.Background())
			t.Errorf("%s: Start succeeded", name)
		}
	}
	if _, err := (&codex.Adapter{Command: []string{"/nonexistent/codex-binary"}}).Start(context.Background(), good, &harnesstest.Recorder{}); err == nil {
		t.Error("a missing binary must fail Start")
	}
}

func TestReadOnlySandboxIsSelectable(t *testing.T) {
	spec := startSpec(t, "x")
	spec.Extra = json.RawMessage(`{"resume_thread":"thr_old","sandbox":"read-only","approval_policy":"untrusted"}`)
	s, rec := begin(t, adapter(t, "resume"), spec)
	finish(t, s)
	joined := strings.Join(harnesstest.Texts(rec.Snapshot()), "\n")
	if !strings.Contains(joined, `"type":"readOnly"`) || !strings.Contains(joined, `"approvalPolicy":"untrusted"`) {
		t.Fatalf("turn/start params do not reflect the options: %s", joined)
	}
}

// ---- failing controls: the suite must be able to fail on this adapter ----

type sinkMut func(next harness.Sink) harness.Sink

type fnSink func(context.Context, harness.Event) error

func (f fnSink) Emit(ctx context.Context, e harness.Event) error { return f(ctx, e) }

type mutAdapter struct {
	harness.Adapter
	sink sinkMut
	sess func(harness.Session) harness.Session
}

func (m mutAdapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	if m.sink != nil {
		sink = m.sink(sink)
	}
	s, err := m.Adapter.Start(ctx, spec, sink)
	if err == nil && m.sess != nil {
		s = m.sess(s)
	}
	return s, err
}

func mutate(c harnesstest.Case, wrap func(harness.Adapter) harness.Adapter) harnesstest.Case {
	return harnesstest.Case{
		Normal:  func(t harnesstest.T) harness.Adapter { return wrap(c.Normal(t)) },
		Unknown: func(t harnesstest.T) harness.Adapter { return wrap(c.Unknown(t)) },
		NoFinal: func(t harnesstest.T) harness.Adapter { return wrap(c.NoFinal(t)) },
		Secret: func(t harnesstest.T) (harness.Adapter, string) {
			a, s := c.Secret(t)
			return wrap(a), s
		},
		Block: func(t harnesstest.T) harness.Adapter { return wrap(c.Block(t)) },
	}
}

type dropInputs struct{ harness.Session }

var dropCount int

func (d dropInputs) Input(ctx context.Context, m string) error {
	dropCount++
	if dropCount%2 == 0 {
		return nil // accepted, never delivered
	}
	return d.Session.Input(ctx, m)
}

// successWait reports every ending as success, like an adapter that treats a
// process exit as completion.
type successWait struct{ harness.Session }

func (s successWait) Wait(ctx context.Context) (harness.Final, error) {
	f, err := s.Session.Wait(ctx)
	if f.Outcome == harness.OutcomeFailed {
		f = harness.Final{Outcome: harness.OutcomeDone}
	}
	return f, err
}

type interruptTwiceErr struct {
	harness.Session
	n int
}

func (d *interruptTwiceErr) Interrupt(ctx context.Context) error {
	d.n++
	if d.n > 1 {
		return errors.New("already interrupted")
	}
	return d.Session.Interrupt(ctx)
}

func TestControlsTheSuiteFailsOnBrokenVariants(t *testing.T) {
	base := conformanceCase()
	if v := harnesstest.Violations(t.TempDir(), base); len(v) != 0 {
		t.Fatalf("the unmodified adapter has violations:\n%s", harnesstest.Joined(v))
	}
	controls := map[string]struct {
		want string
		wrap func(harness.Adapter) harness.Adapter
	}{
		"drops unknown events": {"unknown_event_is_data", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sink: func(n harness.Sink) harness.Sink {
				return fnSink(func(ctx context.Context, e harness.Event) error {
					if e.Kind == harness.KindUnknown {
						return nil
					}
					return n.Emit(ctx, e)
				})
			}}
		}},
		"reports success on exit without result": {"exit_without_result_is_failed", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sink: func(n harness.Sink) harness.Sink {
				return fnSink(func(ctx context.Context, e harness.Event) error {
					if e.Kind == harness.KindFinal {
						e.Payload = json.RawMessage(`{"outcome":"done","exit_code":0}`)
					}
					return n.Emit(ctx, e)
				})
			}, sess: func(s harness.Session) harness.Session { return successWait{s} }}
		}},
		"skips redaction": {"secrets_never_leave", func(a harness.Adapter) harness.Adapter {
			if ca, ok := a.(*codex.Adapter); ok {
				ca.Redact = noRedact{}
			}
			return a
		}},
		"loses queued input": {"lifecycle", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sess: func(s harness.Session) harness.Session { return dropInputs{s} }}
		}},
		"errors on a repeated interrupt": {"lifecycle", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sess: func(s harness.Session) harness.Session { return &interruptTwiceErr{Session: s} }}
		}},
		"emits a second final": {"normal_stream", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sink: func(n harness.Sink) harness.Sink {
				return fnSink(func(ctx context.Context, e harness.Event) error {
					if err := n.Emit(ctx, e); err != nil || e.Kind != harness.KindFinal {
						return err
					}
					e.Seq++
					return n.Emit(ctx, e)
				})
			}}
		}},
		"leaves a seq gap": {"normal_stream", func(a harness.Adapter) harness.Adapter {
			return mutAdapter{Adapter: a, sink: func(n harness.Sink) harness.Sink {
				return fnSink(func(ctx context.Context, e harness.Event) error {
					if e.Seq >= 2 {
						e.Seq++
					}
					return n.Emit(ctx, e)
				})
			}}
		}},
	}
	for name, c := range controls {
		dropCount = 0
		v := harnesstest.Violations(t.TempDir(), mutate(conformanceCase(), c.wrap))
		hit := false
		for _, line := range v {
			if strings.Contains(line, c.want) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("control %q: expected a %s violation, got:\n%s", name, c.want, harnesstest.Joined(v))
		}
	}
}

// tb is the part of testing.TB that harnesstest.T also provides.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

type noRedact struct{}

func (noRedact) Redact(s string) string { return s }
