package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

const secret = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"

func fakeCase(interactive bool, br harnesstest.Break) harnesstest.Case {
	mk := func(m harnesstest.Mode) func(harnesstest.T) harness.Adapter {
		return func(harnesstest.T) harness.Adapter {
			return &harnesstest.Fake{AName: "fake", Interactive: interactive, Mode: m, Break: br, Secret: secret}
		}
	}
	return harnesstest.Case{
		Normal:  mk(harnesstest.ModeNormal),
		Unknown: mk(harnesstest.ModeUnknown),
		NoFinal: mk(harnesstest.ModeNoFinal),
		Block:   mk(harnesstest.ModeBlock),
		Secret: func(harnesstest.T) (harness.Adapter, string) {
			return &harnesstest.Fake{AName: "fake", Interactive: interactive, Mode: harnesstest.ModeSecret, Secret: secret}, secret
		},
	}
}

func TestFakeConformsNonInteractive(t *testing.T) { harnesstest.Conformance(t, fakeCase(false, 0)) }
func TestFakeConformsInteractive(t *testing.T)    { harnesstest.Conformance(t, fakeCase(true, 0)) }

func TestRegistry(t *testing.T) {
	r := harness.NewRegistry()
	if err := r.Register(&harnesstest.Fake{AName: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&harnesstest.Fake{AName: "fake"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	for _, bad := range []string{"", "X", "a", "Has Space", "../x", strings.Repeat("a", 40)} {
		if err := r.Register(&harnesstest.Fake{AName: bad}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil adapter accepted")
	}
	if a, ok := r.Get("fake"); !ok || a.Name() != "fake" {
		t.Fatal("Get failed")
	}
	if got := r.Names(); len(got) != 1 || got[0] != "fake" {
		t.Fatalf("Names = %v", got)
	}
}

func TestEmitter(t *testing.T) {
	ctx := context.Background()
	if _, err := harness.NewEmitter("h", "s", nil, harness.PatternRedactor()); err == nil {
		t.Fatal("nil sink accepted")
	}
	rec := &harnesstest.Recorder{}
	em, _ := harness.NewEmitter("h1", "ses_1", rec, harness.PatternRedactor())
	_ = em.Turn(ctx, 1)
	_ = em.Message(ctx, "assistant", "key "+secret+" and postgres://u:p@db/x")
	_ = em.Unknown(ctx, "why", strings.Repeat("é", 5000))
	_ = em.ToolCall(ctx, "id", "shell", []byte("password=hunter2"), "ok")
	if err := em.Final(ctx, harness.Final{Outcome: harness.OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	if err := em.Turn(ctx, 2); !errors.Is(err, harness.ErrAlreadyFinal) {
		t.Fatalf("emit after final: %v", err)
	}
	if err := em.Final(ctx, harness.Final{Outcome: harness.OutcomeDone}); !errors.Is(err, harness.ErrAlreadyFinal) {
		t.Fatalf("second final: %v", err)
	}
	evs := rec.Snapshot()
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		p := string(e.Payload)
		if strings.Contains(p, "sk-ant") || strings.Contains(p, "hunter2") || strings.Contains(p, "u:p@db") {
			t.Fatalf("unredacted payload: %s", p)
		}
		if len(p) > harness.MaxUnknownRaw+512 {
			t.Fatalf("payload not bounded: %d bytes", len(p))
		}
	}
	// the tool input is digested, never carried
	for _, e := range evs {
		if e.Kind == harness.KindToolCall && strings.Contains(string(e.Payload), "password") {
			t.Fatal("tool input leaked into the event")
		}
	}
}

func TestEmitterSinkFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	rec := &harnesstest.Recorder{FailAfter: 1}
	em, _ := harness.NewEmitter("h", "s", rec, harness.PatternRedactor())
	if err := em.Turn(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := em.Message(ctx, "assistant", "x"); err == nil {
		t.Fatal("sink error swallowed")
	}
	if err := em.Message(ctx, "assistant", "y"); err == nil {
		t.Fatal("emit continued after a sink error")
	}
	// final still goes out once the sink recovers, and cannot claim success
	rec.FailAfter = 0
	if err := em.Final(ctx, harness.Final{Outcome: harness.OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	evs := rec.Snapshot()
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Payload), `"failed"`) {
		t.Fatalf("final after a sink error is not failed: %s", last.Payload)
	}
}
