package claudecode

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

// ctxSink behaves like a real database sink: it honours the context it is
// given, and can stall on a chosen message.
type ctxSink struct {
	harnesstest.Recorder
	stallOn string
	stall   time.Duration
	onFirst func() // called once, on the first Emit
	once    sync.Once
}

func (c *ctxSink) Emit(ctx context.Context, e harness.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.onFirst != nil && e.Kind == harness.KindTurn && strings.Contains(string(e.Payload), `"index":2`) {
		c.once.Do(c.onFirst) // the second turn is the Input under test
	}
	if c.stallOn != "" && strings.Contains(string(e.Payload), c.stallOn) {
		time.Sleep(c.stall)
	}
	return c.Recorder.Emit(ctx, e)
}

func init() {
	switch os.Getenv(modeEnv) {
	case "quick":
		emit(assistant(text("stall-here")))
		pad := strings.Repeat("p", 240)
		for i := 0; i < 150; i++ { // ~45 KB: fits in the pipe, so the child exits while the sink still stalls
			emit(assistant(text(pad)))
		}
		emit(assistant(text("three")))
		emit(result("success", false))
		os.Exit(0)
	case "merge":
		// real claude folds inputs queued during a turn into that turn: read
		// one line, wait, take everything already queued, answer once
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		sc.Scan()
		time.Sleep(400 * time.Millisecond)
		emit(assistant(text("merged-answer")))
		emit(result("success", false))
		for sc.Scan() { // the rest are consumed silently, as merged
		}
		os.Exit(0)
	case "lost":
		// answers the first line, reads the second, exits 0 without answering
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		sc.Scan()
		time.Sleep(300 * time.Millisecond)
		emit(assistant(text("first")))
		emit(result("success", false))
		sc.Scan()
		os.Exit(0)
	case "deaf":
		time.Sleep(time.Hour) // never reads stdin
	case "stderrbearer":
		tok := os.Getenv("FAKE_TOKEN")
		n := 16384 + 3 - (len("Bearer ") + len(tok) + 1) - 1
		os.Stderr.WriteString("Bearer " + tok + "\n" + strings.Repeat("y", n) + "\n")
		os.Exit(1)
	}
}

// D1: a cancelled Input must not poison the sink and end the session.
func TestCancelledInputDoesNotEndTheSession(t *testing.T) {
	a := adapter("block", false, nil)
	cctx, cancel := context.WithCancel(context.Background())
	sink := &ctxSink{onFirst: cancel}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_cx", Prompt: "go", Dir: t.TempDir()}, sink)
	if err != nil {
		t.Fatal(err)
	}
	// The caller's context is cancelled while Input is being recorded, as
	// happens when a request is abandoned mid-call.
	_ = s.Input(cctx, "late")
	time.Sleep(200 * time.Millisecond)
	ctx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	if err := s.Input(ctx, "still-alive"); err != nil {
		t.Fatalf("session ended after a cancelled Input: %v", err)
	}
	if !sink.WaitFor(5*time.Second, func(e []harness.Event) bool { return contains(harnesstest.Echoes(e), "still-alive") }) {
		t.Fatal("the session stopped answering after a cancelled Input")
	}
	_ = s.Stop(ctx)
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeStopped {
		t.Fatalf("%+v", f)
	}
}

// D2: a slow sink must not turn a finished run into a failure.
func TestSlowSinkDoesNotLoseTheResult(t *testing.T) {
	a := execAdapter("quick", nil)
	a.DrainGrace = 300 * time.Millisecond
	sink := &ctxSink{stallOn: "stall-here", stall: 2 * time.Second}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_slow", Prompt: "go", Dir: t.TempDir()}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("a stalled sink lost the result: %+v", f)
	}
	if !contains(harnesstest.Texts(sink.Snapshot()), "three") {
		t.Fatal("events after the stalled one were dropped")
	}
}

// D3: a credential cut by the stderr window must not survive in the reason.
func TestStderrCutInsideBearerTokenDoesNotLeak(t *testing.T) {
	tok := strings.Repeat("Ab1x", 12)[:40]
	f, _ := runToEnd(t, execAdapter("stderrbearer", map[string]string{"FAKE_TOKEN": tok}), harness.StartSpec{Prompt: "go"})
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("%+v", f)
	}
	if strings.Contains(f.Reason, tok[:12]) {
		t.Fatalf("token survives in %q", f.Reason)
	}
}

// D4 and D5: with the child not reading, a refused Input leaves no record, and
// a second Input still honours its own context.
func TestRefusedInputIsNotRecordedAndContextsAreIndependent(t *testing.T) {
	a := adapter("deaf", false, nil)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_ref", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 1<<20)
	accepted := 1 // the prompt
	for i := 0; i < 400; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		err := s.Input(ctx, big)
		cancel()
		if err != nil {
			break
		}
		accepted++
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // holds the send slot for a while
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = s.Input(ctx, big)
	}()
	time.Sleep(100 * time.Millisecond)
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_ = s.Input(ctx, big)
	cancel()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("second Input took %v: it did not honour its own context", d)
	}
	wg.Wait()
	sctx, c2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer c2()
	if err := s.Stop(sctx); err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, e := range rec.Snapshot() {
		if e.Kind == harness.KindMessage && strings.Contains(string(e.Payload), `"role":"user"`) {
			users++
		}
	}
	f, _ := s.Wait(sctx)
	if users != accepted || f.Reason != "undelivered_inputs="+strconv.Itoa(accepted) {
		t.Fatalf("accepted %d, recorded %d user messages, final %+v", accepted, users, f)
	}
}

// A graceful Stop closes stdin and the child exits 0 by itself, well before
// the kill timer.
func TestGracefulStopClosesStdin(t *testing.T) {
	a := adapter("silent", false, nil).(*Adapter)
	a.StopGrace = 20 * time.Second
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_gs", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop needed the kill timer: %v", err)
	}
	f, _ := s.Wait(ctx)
	if f.ExitCode != 0 || time.Since(t0) > 5*time.Second {
		t.Fatalf("not a graceful exit: %+v after %v", f, time.Since(t0))
	}
}

// Inputs queued while a turn runs are merged by real claude into that turn:
// one result answers them all, so none is reported undelivered.
func TestMergedInputsAreNotReportedUndelivered(t *testing.T) {
	a := adapter("merge", false, nil).(*Adapter)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_mg", Prompt: "one", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Input(ctx, "two")
	_ = s.Input(ctx, "three")
	if !rec.WaitFor(5*time.Second, func(e []harness.Event) bool { return contains(harnesstest.Texts(e), "merged-answer") }) {
		t.Fatal("no answer")
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := s.Wait(ctx)
	if f.Outcome != harness.OutcomeStopped || strings.Contains(f.Reason, "undelivered") || !strings.Contains(f.Reason, "inputs=3 results=1") {
		t.Fatalf("merged inputs must be noted, not reported as undelivered: %+v", f)
	}
}

// A child that answers the first prompt and exits without answering a second
// input must not look like a clean success: the unanswered input is noted.
func TestLostInputIsNotSilent(t *testing.T) {
	a := adapter("lost", false, nil).(*Adapter)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_lost", Prompt: "one", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Input(ctx, "two")
	f, _ := s.Wait(ctx)
	if !strings.Contains(f.Reason, "inputs=2 results=1") {
		t.Fatalf("a lost input left no trace in the final: %+v", f)
	}
}

func TestCancelledInputIsRefusedBeforeRecording(t *testing.T) {
	a := adapter("block", false, nil)
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_cn", Prompt: "go", Dir: t.TempDir()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 50; i++ { // the select race made this pass only some of the time
		if err := s.Input(ctx, "never"); err == nil {
			t.Fatal("a cancelled Input was accepted")
		}
	}
	sctx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	_ = s.Stop(sctx)
	if contains(harnesstest.Texts(rec.Snapshot()), "never") {
		t.Fatal("a refused Input was recorded as a turn")
	}
}
