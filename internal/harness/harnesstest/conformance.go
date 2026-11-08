package harnesstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"fabric/internal/harness"
)

// Case supplies adapters primed to behave in the way each scenario needs.
// An adapter's fixture (recorded output, scripted fake process) is how a real
// adapter reaches these behaviours without a network or a model. Nil fields
// are failures, not skips: a scenario an adapter cannot reach is a gap in its
// fixtures.
type Case struct {
	// Normal: one turn with a message, a tool call and usage, then success.
	Normal func(t T) harness.Adapter
	// Unknown: Normal plus one harness event the adapter cannot classify.
	Unknown func(t T) harness.Adapter
	// NoFinal: the harness exits with a non-zero status and no result.
	NoFinal func(t T) harness.Adapter
	// Secret: a message carries the returned secret-shaped string.
	Secret func(t T) (a harness.Adapter, secret string)
	// Block: starts a turn and runs until Interrupt or Stop. If the adapter
	// is interactive, each Input is answered with a Message "echo:<input>".
	Block func(t T) harness.Adapter
	// Optional lists event kinds a harness does not report at all (for
	// example kimi gives no usage). Every other kind must appear in a normal run.
	Optional []harness.Kind
}

const waitFor = 5 * time.Second

func start(t T, a harness.Adapter, sink harness.Sink) harness.Session {
	t.Helper()
	s, err := a.Start(context.Background(), harness.StartSpec{SessionID: "ses_" + strings.ReplaceAll(t.Name(), "/", "_"), Prompt: "go", Dir: t.TempDir()}, sink)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

func wait(t T, s harness.Session) harness.Final {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return f
}

// CheckStream verifies the stream invariants of one finished session.
func CheckStream(evs []harness.Event) error {
	if len(evs) == 0 {
		return errors.New("no events")
	}
	finals := 0
	for i, e := range evs {
		if e.Version != harness.EventVersion {
			return fmt.Errorf("event %d: version %d", i, e.Version)
		}
		if e.Seq != int64(i+1) {
			return fmt.Errorf("event %d: seq %d, want %d (gap or reorder)", i, e.Seq, i+1)
		}
		if e.Harness == "" || e.Session == "" {
			return fmt.Errorf("event %d: missing harness or session", i)
		}
		if e.Harness != evs[0].Harness || e.Session != evs[0].Session {
			return fmt.Errorf("event %d: harness or session changed mid-stream", i)
		}
		var dst any
		switch e.Kind {
		case harness.KindTurn:
			dst = &harness.Turn{}
		case harness.KindToolCall:
			dst = &harness.ToolCall{}
		case harness.KindMessage:
			dst = &harness.Message{}
		case harness.KindUsage:
			dst = &harness.Usage{}
		case harness.KindFinal:
			dst = &harness.Final{}
			finals++
		case harness.KindUnknown:
			dst = &harness.Unknown{}
		default:
			return fmt.Errorf("event %d: unknown kind %q", i, e.Kind)
		}
		if err := json.Unmarshal(e.Payload, dst); err != nil {
			return fmt.Errorf("event %d (%s): payload does not decode: %v", i, e.Kind, err)
		}
	}
	if finals != 1 {
		return fmt.Errorf("%d final events, want exactly 1", finals)
	}
	if evs[len(evs)-1].Kind != harness.KindFinal {
		return errors.New("final is not the last event")
	}
	return nil
}

func optional(ks []harness.Kind, k harness.Kind) bool {
	for _, o := range ks {
		if o == k {
			return true
		}
	}
	return false
}

// sameFinal checks that what Wait reports is what the stream recorded: a sink
// stores the stream, so a divergence would give the caller one answer and the
// record another.
func sameFinal(t T, evs []harness.Event, f harness.Final) {
	t.Helper()
	got, ok := finalOf(evs)
	if !ok {
		t.Fatalf("no final event in the stream")
		return
	}
	if got.Outcome != f.Outcome || got.ExitCode != f.ExitCode {
		t.Fatalf("stream final %+v differs from Wait %+v", got, f)
	}
}

func finalOf(evs []harness.Event) (harness.Final, bool) {
	for _, e := range evs {
		if e.Kind == harness.KindFinal {
			var f harness.Final
			_ = json.Unmarshal(e.Payload, &f)
			return f, true
		}
	}
	return harness.Final{}, false
}

// Conformance runs the contract against an adapter family.
func Conformance(t *testing.T, c Case) { run(tbT{t}, c) }

func run(t T, c Case) {
	t.Helper()
	need := map[string]bool{"Normal": c.Normal != nil, "Unknown": c.Unknown != nil, "NoFinal": c.NoFinal != nil, "Secret": c.Secret != nil, "Block": c.Block != nil}
	for n, ok := range need {
		if !ok {
			t.Fatalf("conformance case is missing %s", n)
		}
	}

	t.Run("normal_stream", func(t T) {
		rec := &Recorder{}
		s := start(t, c.Normal(t), rec)
		f := wait(t, s)
		evs := rec.Snapshot()
		if err := CheckStream(evs); err != nil {
			t.Fatal(err)
		}
		if f.Outcome != harness.OutcomeDone {
			t.Fatalf("outcome %q, want done", f.Outcome)
		}
		have := map[harness.Kind]bool{}
		for _, e := range evs {
			have[e.Kind] = true
		}
		for _, k := range []harness.Kind{harness.KindTurn, harness.KindMessage, harness.KindToolCall, harness.KindUsage} {
			if !have[k] && !optional(c.Optional, k) {
				t.Errorf("normal run emitted no %s event", k)
			}
		}
		sameFinal(t, evs, f)
	})

	t.Run("unknown_event_is_data", func(t T) {
		rec := &Recorder{}
		s := start(t, c.Unknown(t), rec)
		f := wait(t, s)
		evs := rec.Snapshot()
		if err := CheckStream(evs); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range evs {
			if e.Kind == harness.KindUnknown {
				n++
			}
		}
		if n == 0 {
			t.Fatal("an unclassifiable harness event was dropped instead of becoming an unknown event")
		}
		if f.Outcome != harness.OutcomeDone {
			t.Fatalf("an unknown event changed the outcome to %q", f.Outcome)
		}
		sameFinal(t, evs, f)
	})

	t.Run("exit_without_result_is_failed", func(t T) {
		rec := &Recorder{}
		s := start(t, c.NoFinal(t), rec)
		f := wait(t, s)
		if err := CheckStream(rec.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if f.Outcome != harness.OutcomeFailed {
			t.Fatalf("outcome %q, want failed", f.Outcome)
		}
		if f.ExitCode == 0 || f.Reason == "" {
			t.Fatalf("a failure must carry the exit status and a reason: %+v", f)
		}
		sameFinal(t, rec.Snapshot(), f)
	})

	t.Run("secrets_never_leave", func(t T) {
		a, secret := c.Secret(t)
		rec := &Recorder{}
		s := start(t, a, rec)
		f := wait(t, s)
		evs := rec.Snapshot()
		marker := false
		for _, e := range evs {
			p := string(e.Payload)
			if strings.Contains(p, "[REDACTED]") {
				marker = true
			}
			// no window of the secret may survive, so a redactor that keeps
			// the tail (or the head) of a key is caught
			for i := 0; i+5 <= len(secret); i++ {
				if strings.Contains(p, secret[i:i+5]) {
					t.Fatalf("event %d (%s) carries part of the secret: %q", e.Seq, e.Kind, secret[i:i+5])
				}
			}
		}
		if !marker {
			t.Fatalf("no [REDACTED] marker in the stream: the scenario never exercised redaction")
		}
		sameFinal(t, evs, f)
	})

	t.Run("sink_failure_is_not_success", func(t T) {
		rec := &Recorder{FailAfter: 2}
		s := start(t, c.Normal(t), rec)
		f := wait(t, s)
		if f.Outcome == harness.OutcomeDone {
			t.Fatal("a run whose events could not be recorded reported success")
		}
		evs := rec.Snapshot()
		for i, e := range evs {
			if e.Seq != int64(i+1) {
				t.Fatalf("seq gap after a sink failure: event %d has seq %d", i, e.Seq)
			}
		}
	})

	t.Run("stop_ends_a_running_session", func(t T) {
		rec := &Recorder{}
		s := start(t, c.Block(t), rec)
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()
		if !rec.WaitFor(waitFor, func(e []harness.Event) bool { return len(e) >= 1 }) {
			t.Fatalf("blocked session emitted nothing")
		}
		if err := s.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		f := wait(t, s)
		evs := rec.Snapshot()
		if err := CheckStream(evs); err != nil {
			t.Fatalf("%v", err)
		}
		if f.Outcome != harness.OutcomeStopped {
			t.Fatalf("a stopped session ended %q, want stopped", f.Outcome)
		}
		sameFinal(t, evs, f)
	})

	t.Run("lifecycle", func(t T) {
		a := c.Block(t)
		interactive := a.Caps().Interactive
		rec := &Recorder{}
		s := start(t, a, rec)
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()
		if !rec.WaitFor(waitFor, func(e []harness.Event) bool { return len(e) >= 1 }) {
			t.Fatal("blocked session emitted nothing")
		}

		if !interactive {
			if err := s.Input(ctx, "x"); !errors.Is(err, harness.ErrNotInteractive) {
				t.Fatalf("Input on a non-interactive session: %v, want ErrNotInteractive", err)
			}
		} else {
			const n = 5
			for i := 0; i < n; i++ {
				if err := s.Input(ctx, "m"+strconv.Itoa(i)); err != nil {
					t.Fatalf("Input %d: %v", i, err)
				}
			}
			if !rec.WaitFor(waitFor, func(e []harness.Event) bool { return len(Echoes(e)) >= n }) {
				t.Fatalf("%d inputs accepted while idle, only %d answered: input was lost silently", n, len(Echoes(rec.Snapshot())))
			}
			// Interrupt while inputs may still be pending: the session must survive.
			if err := s.Interrupt(ctx); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			if err := s.Interrupt(ctx); err != nil {
				t.Fatalf("second Interrupt must be harmless: %v", err)
			}
			if err := s.Input(ctx, "after-interrupt"); err != nil {
				t.Fatalf("Input after Interrupt: %v", err)
			}
		}
		if !interactive {
			if err := s.Interrupt(ctx); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			if err := s.Interrupt(ctx); err != nil {
				t.Fatalf("second Interrupt must be harmless: %v", err)
			}
			f := wait(t, s)
			if f.Outcome != harness.OutcomeInterrupted {
				t.Fatalf("interrupt of a non-interactive session ended %q", f.Outcome)
			}
		} else {
			// Stop immediately: every accepted input is either answered or
			// counted as undelivered in the final reason. None vanishes.
			if err := s.Stop(ctx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		}
		if err := s.Stop(ctx); err != nil {
			t.Fatalf("second Stop must be harmless: %v", err)
		}
		f := wait(t, s)
		evs := rec.Snapshot()
		if err := CheckStream(evs); err != nil {
			t.Fatal(err)
		}
		sameFinal(t, evs, f)
		if interactive {
			if f.Outcome != harness.OutcomeStopped {
				t.Fatalf("outcome %q, want stopped", f.Outcome)
			}
			sent := 6 // five inputs plus after-interrupt
			undelivered := 0
			if i := strings.Index(f.Reason, "undelivered_inputs="); i >= 0 {
				rest := f.Reason[i+len("undelivered_inputs="):]
				if j := strings.IndexAny(rest, " ;,"); j >= 0 {
					rest = rest[:j]
				}
				undelivered, _ = strconv.Atoi(rest)
			}
			got := Echoes(evs)
			if len(got)+undelivered != sent {
				t.Fatalf("%d inputs accepted, %d answered, %d reported undelivered: input was lost silently", sent, len(got), undelivered)
			}
			want := []string{"m0", "m1", "m2", "m3", "m4", "after-interrupt"}
			for i, g := range got {
				if g != want[i] {
					t.Fatalf("input order broken: answer %d is %q, want %q", i, g, want[i])
				}
			}
		}
		err := s.Input(ctx, "late")
		want := harness.ErrSessionEnded
		if !interactive {
			want = harness.ErrNotInteractive
		}
		if !errors.Is(err, want) {
			t.Fatalf("Input after the end: %v, want %v", err, want)
		}
		if n := len(rec.Snapshot()); n != len(evs) {
			t.Fatalf("events emitted after final: %d -> %d", len(evs), n)
		}
	})
}
