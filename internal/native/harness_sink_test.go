package native_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
)

func steps() []fakeapi.Step {
	return []fakeapi.Step{
		{Events: fakeapi.ToolUse("", "", "toolu_r", "read", `{"path":"notes.txt"}`, 120, 40)},
		{Events: fakeapi.ToolUse("", "", "toolu_s", "shell", `{"command":"echo done"}`, 200, 40)},
		{Events: fakeapi.Text("finished", 260, 10)},
	}
}

// A real mission run, with a Sink, produces a conforming harness trace.
func TestRunWithSinkRecordsAConformingTrace(t *testing.T) {
	e := newEnv(t, steps()...)
	st := storetest.Open(t)
	rec := &harnesstest.Recorder{}
	o := opts(true)
	o.Sink = rec
	r, err := native.Run(context.Background(), st, e.g, o)
	if err != nil || r.Outcome != "done" {
		t.Fatalf("receipt %+v err %v", r, err)
	}
	evs := rec.Snapshot()
	if err := harness.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	kinds := map[harness.Kind]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
		if strings.Contains(string(ev.Payload), canary) {
			t.Fatalf("API key in event %d", ev.Seq)
		}
	}
	if kinds[harness.KindTurn] != 3 || kinds[harness.KindUsage] != 3 || kinds[harness.KindFinal] != 1 {
		t.Fatalf("kinds %v", kinds)
	}
	var done []string
	for _, ev := range evs {
		if ev.Kind == harness.KindToolCall {
			var tc harness.ToolCall
			_ = json.Unmarshal(ev.Payload, &tc)
			if tc.Status == "ok" {
				done = append(done, tc.ID+":"+tc.Name)
			}
		}
	}
	if len(done) != 2 || done[0] != "toolu_r:read" || done[1] != "toolu_s:shell" {
		t.Fatalf("finished tool calls %v", done)
	}
	var f harness.Final
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &f)
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("final %+v", f)
	}
}

// Without a Sink nothing changes: no observer is built, the receipt is the same.
func TestRunWithoutSinkIsUnchanged(t *testing.T) {
	e := newEnv(t, steps()...)
	st := storetest.Open(t)
	r, err := native.Run(context.Background(), st, e.g, opts(true))
	if err != nil || r.Outcome != "done" || r.Turns != 3 {
		t.Fatalf("receipt %+v err %v", r, err)
	}
}

// A sink that cannot record stops the run before the first paid request.
func TestRunWithFailingSinkSendsNothing(t *testing.T) {
	e := newEnv(t, steps()...)
	st := storetest.Open(t)
	rec := &harnesstest.Recorder{FailNow: true}
	o := opts(true)
	o.Sink = rec
	if _, err := native.Run(context.Background(), st, e.g, o); err == nil {
		t.Fatal("run succeeded although no event could be recorded")
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Fatalf("%d requests sent with an unrecordable trace", n)
	}
}

// A secret-shaped string the model repeats (the active API key itself is
// already refused by the provider layer) is redacted from events.
func TestRunRedactsKeyInEvents(t *testing.T) {
	e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("db is postgres://svc:hunter2pw@db.internal/app ok", 20, 10)})
	st := storetest.Open(t)
	rec := &harnesstest.Recorder{}
	o := opts(true)
	o.Sink = rec
	if _, err := native.Run(context.Background(), st, e.g, o); err != nil {
		t.Fatal(err)
	}
	marker := false
	for _, ev := range rec.Snapshot() {
		p := string(ev.Payload)
		if strings.Contains(p, "hunter2pw") || strings.Contains(p, "db.internal") {
			t.Fatalf("key material in event %d: %s", ev.Seq, p)
		}
		marker = marker || strings.Contains(p, "[REDACTED")
	}
	if !marker {
		t.Fatal("no redaction marker: the secret never reached an event, so the test proves nothing")
	}
}

type cancelSink struct {
	harnesstest.Recorder
	cancel context.CancelFunc
}

func (c *cancelSink) Emit(ctx context.Context, e harness.Event) error {
	err := c.Recorder.Emit(ctx, e)
	if e.Kind == harness.KindTurn {
		c.cancel() // Stop arrives while the first request is being made
	}
	return err
}

// Cancelling the caller's context (fab harness run on SIGINT) settles the
// trace as stopped, with a conforming stream.
func TestRunStoppedByContextSettlesAsStopped(t *testing.T) {
	e := newEnv(t, steps()...)
	st := storetest.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &cancelSink{cancel: cancel}
	o := opts(true)
	o.Sink = sink
	_, _ = native.Run(ctx, st, e.g, o)
	evs := sink.Snapshot()
	if err := harness.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	var f harness.Final
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &f)
	if f.Outcome != harness.OutcomeStopped {
		t.Fatalf("final %+v, want stopped", f)
	}
}
