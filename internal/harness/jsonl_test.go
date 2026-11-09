package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fabric/internal/harness"
)

func TestJSONLSinkRoundTripAndModes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	s, err := harness.NewJSONLSink(p)
	if err != nil {
		t.Fatal(err)
	}
	em, _ := harness.NewEmitter("h", "ses_1", s, harness.PatternRedactor())
	ctx := context.Background()
	_ = em.Turn(ctx, 1)
	_ = em.Message(ctx, "assistant", "hello")
	_ = em.Final(ctx, harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", st.Mode(), err)
	}
	evs, err := harness.ReadJSONL(p)
	if err != nil || len(evs) != 3 {
		t.Fatalf("read %d events err %v", len(evs), err)
	}
	if err := harness.CheckStream(evs); err != nil {
		t.Fatal(err)
	}
	// a second run never appends to an existing trace
	if _, err := harness.NewJSONLSink(p); err == nil {
		t.Fatal("existing trace file reopened")
	}
}

func TestCheckStreamRejectsBrokenTraces(t *testing.T) {
	good := func() []harness.Event {
		p := filepath.Join(t.TempDir(), "t.jsonl")
		s, _ := harness.NewJSONLSink(p)
		em, _ := harness.NewEmitter("h", "ses_1", s, harness.PatternRedactor())
		_ = em.Turn(context.Background(), 1)
		_ = em.Final(context.Background(), harness.Final{Outcome: harness.OutcomeDone})
		_ = s.Close()
		evs, _ := harness.ReadJSONL(p)
		return evs
	}
	if err := harness.CheckStream(good()); err != nil {
		t.Fatal(err)
	}
	e := good()
	e[1].Seq = 5
	if harness.CheckStream(e) == nil {
		t.Fatal("seq gap accepted")
	}
	e = good()
	e = append(e, e[0])
	e[2].Seq = 3
	if harness.CheckStream(e) == nil {
		t.Fatal("event after final accepted")
	}
	if harness.CheckStream(good()[:1]) == nil {
		t.Fatal("trace without final accepted")
	}
	if harness.CheckStream(nil) == nil {
		t.Fatal("empty trace accepted")
	}
}
