package harness_test

import (
	"context"
	"strings"
	"testing"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

func TestEmitterPanickingSinkIsAnErrorNotAGap(t *testing.T) {
	ctx := context.Background()
	rec := &harnesstest.Recorder{PanicAfter: 2}
	em, _ := harness.NewEmitter("h", "s", rec, harness.PatternRedactor())
	_ = em.Turn(ctx, 1)
	_ = em.Message(ctx, "assistant", "x")
	if err := em.Message(ctx, "assistant", "y"); err == nil {
		t.Fatal("sink panic was not reported")
	}
	f, _ := em.FinalOutcome(ctx, harness.Final{Outcome: harness.OutcomeDone})
	if f.Outcome == harness.OutcomeDone {
		t.Fatal("final reported success after a sink panic")
	}
	for i, e := range rec.Snapshot() {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq gap after a sink panic: %d at index %d", e.Seq, i)
		}
	}
}

// Final must not claim success when an earlier emit failed.
func TestFinalOutcomeIsDecidedUnderTheLock(t *testing.T) {
	ctx := context.Background()
	rec := &harnesstest.Recorder{FailNow: true}
	em, _ := harness.NewEmitter("h", "s", rec, harness.PatternRedactor())
	_ = em.Turn(ctx, 1) // fails; recorded as the sticky error
	rec.FailNow = false
	f, err := em.FinalOutcome(ctx, harness.Final{Outcome: harness.OutcomeDone})
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != harness.OutcomeFailed {
		t.Fatalf("outcome %q, want failed", f.Outcome)
	}
	evs := rec.Snapshot()
	if len(evs) != 1 || evs[0].Seq != 1 || !strings.Contains(string(evs[0].Payload), `"failed"`) {
		t.Fatalf("recorded %v", evs)
	}
}

// Secret shapes are assembled at run time so no key-shaped literal sits in the source.
func TestPatternRedactorShapes(t *testing.T) {
	cases := []string{
		`{"api_key": "abc123def456ghi789"}`,
		"sk_" + "live_abcdefghijklmnop1234",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\n-----END RSA PRIVATE KEY-----",
		"xox" + "b-123456789012-abcdefghij",
		"AIza" + "SyA1234567890abcdefghijklmnopqrstuv",
		"git" + "hub_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"mysql://user:hunter2pass@db.example/x",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==",
		"redis://:p4ssw0rd@cache:6379",
		"pass​word=hunter2",
		"api_​key: sekret123",
	}
	r := harness.PatternRedactor()
	for _, c := range cases {
		got := r.Redact(c)
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("not redacted: %q -> %q", c, got)
		}
		for _, leak := range []string{"hunter2", "abc123def456", "sekret123", "p4ssw0rd", "dXNlcjpwYXNzd29yZA", "MIIEabc"} {
			if strings.Contains(got, leak) {
				t.Errorf("leaked %q in %q", leak, got)
			}
		}
	}
}

func TestRegistryRejectsTypedNil(t *testing.T) {
	var f *harnesstest.Fake
	if err := harness.NewRegistry().Register(f); err == nil {
		t.Fatal("typed nil adapter registered")
	}
}
