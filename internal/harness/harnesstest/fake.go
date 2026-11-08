// Package harnesstest holds the conformance suite every harness adapter must
// pass, a recording sink, and a scriptable fake adapter that proves the suite
// can fail (see mutants_test.go in package harness).
package harnesstest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"fabric/internal/harness"
)

// Mode is what a fake session does.
type Mode int

const (
	ModeNormal  Mode = iota // one turn, message, tool call, usage, done
	ModeUnknown             // like Normal plus one unclassifiable harness event
	ModeNoFinal             // the harness exits 3 without reporting a result
	ModeSecret              // a message carries Fake.Secret
	ModeBlock               // starts a turn and waits for Interrupt, Stop or Input
)

// Break injects a session-level contract violation, for mutant tests only.
type Break int

const (
	BreakNone Break = iota
	BreakSuccessOnNoFinal
	BreakDropInput
	BreakInputAfterEndOK
	BreakInterruptTwiceErr
	BreakStopTwiceErr
)

// Fake is a scriptable adapter built on harness.Emitter.
type Fake struct {
	AName       string
	Interactive bool
	Mode        Mode
	Break       Break
	Secret      string
	NoRedact    bool // mutant: send text through unredacted
}

func (f *Fake) Name() string { return f.AName }
func (f *Fake) Caps() harness.Caps {
	return harness.Caps{Interactive: f.Interactive}
}

type fakeSession struct {
	f     *Fake
	id    string
	em    *harness.Emitter
	inbox chan string
	stop  chan struct{}
	intr  chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	ended    bool
	stopOnce sync.Once
	intrSeen bool
	final    harness.Final
}

// Start implements harness.Adapter.
func (f *Fake) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	var red harness.Redactor = harness.PatternRedactor()
	if f.NoRedact {
		red = identity{}
	}
	em, err := harness.NewEmitter(f.AName, spec.SessionID, sink, red)
	if err != nil {
		return nil, err
	}
	s := &fakeSession{f: f, id: spec.SessionID, em: em, inbox: make(chan string, 64),
		stop: make(chan struct{}), intr: make(chan struct{}, 1), done: make(chan struct{})}
	go s.run()
	return s, nil
}

func (s *fakeSession) ID() string { return s.id }

func (s *fakeSession) finish(f harness.Final) {
	s.mu.Lock()
	s.ended = true
	s.final = f
	s.mu.Unlock()
	_ = s.em.Final(context.Background(), f)
	s.mu.Lock()
	s.final.Outcome = s.finalOutcome(f)
	s.mu.Unlock()
	close(s.done)
}

// finalOutcome mirrors the emitter's rewrite so Wait reports what was emitted.
func (s *fakeSession) finalOutcome(f harness.Final) string {
	if s.em.Err() != nil && f.Outcome == harness.OutcomeDone {
		return harness.OutcomeFailed
	}
	return f.Outcome
}

func (s *fakeSession) run() {
	ctx := context.Background()
	_ = s.em.Turn(ctx, 1)
	switch s.f.Mode {
	case ModeSecret:
		_ = s.em.Message(ctx, "assistant", "found "+s.f.Secret+" in env")
	default:
		_ = s.em.Message(ctx, "assistant", "hello")
	}
	if s.f.Mode == ModeBlock {
		s.loop()
		return
	}
	_ = s.em.ToolCall(ctx, "t1", "shell", []byte(`{"cmd":"ls"}`), "ok")
	if s.f.Mode == ModeUnknown {
		_ = s.em.Unknown(ctx, "unrecognised harness event type", `{"type":"weird","x":1}`)
	}
	_ = s.em.Usage(ctx, harness.Usage{Input: 10, Output: 5})
	if s.f.Mode == ModeNoFinal {
		if s.f.Break == BreakSuccessOnNoFinal {
			s.finish(harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0})
			return
		}
		s.finish(harness.Final{Outcome: harness.OutcomeFailed, Reason: "harness exited without a result", ExitCode: 3})
		return
	}
	s.finish(harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0})
}

func (s *fakeSession) loop() {
	ctx := context.Background()
	n := 0
	for {
		select {
		case m := <-s.inbox:
			n++
			if s.f.Break == BreakDropInput && n%2 == 0 {
				continue
			}
			_ = s.em.Message(ctx, "assistant", "echo:"+m)
		case <-s.intr:
			if s.f.Interactive {
				_ = s.em.Message(ctx, "system", "turn interrupted")
				continue
			}
			s.finish(harness.Final{Outcome: harness.OutcomeInterrupted, ExitCode: -1})
			return
		case <-s.stop:
			reason := ""
			if left := len(s.inbox); left > 0 {
				reason = fmt.Sprintf("undelivered_inputs=%d", left)
			}
			s.finish(harness.Final{Outcome: harness.OutcomeStopped, Reason: reason, ExitCode: -1})
			return
		}
	}
}

func (s *fakeSession) Input(ctx context.Context, msg string) error {
	if !s.f.Interactive {
		return harness.ErrNotInteractive
	}
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended && s.f.Break != BreakInputAfterEndOK {
		return harness.ErrSessionEnded
	}
	select {
	case s.inbox <- msg:
		return nil
	default:
		return fmt.Errorf("harness: input queue full")
	}
}

func (s *fakeSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	seen := s.intrSeen
	s.intrSeen = true
	ended := s.ended
	s.mu.Unlock()
	if seen && s.f.Break == BreakInterruptTwiceErr {
		return fmt.Errorf("already interrupted")
	}
	if ended {
		return nil
	}
	select {
	case s.intr <- struct{}{}:
	default:
	}
	return nil
}

func (s *fakeSession) Stop(ctx context.Context) error {
	first := false
	s.stopOnce.Do(func() { first = true; close(s.stop) })
	if !first && s.f.Break == BreakStopTwiceErr {
		return fmt.Errorf("already stopped")
	}
	_, err := s.Wait(ctx)
	return err
}

func (s *fakeSession) Wait(ctx context.Context) (harness.Final, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.final, nil
	case <-ctx.Done():
		return harness.Final{}, ctx.Err()
	}
}

// Recorder is a Sink that keeps every event and can be told to fail.
type Recorder struct {
	mu     sync.Mutex
	Events []harness.Event
	// FailAfter, when > 0, makes Emit fail once that many events are stored.
	FailAfter int
}

// Emit implements harness.Sink.
func (r *Recorder) Emit(ctx context.Context, e harness.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FailAfter > 0 && len(r.Events) >= r.FailAfter {
		return fmt.Errorf("recorder: store unavailable")
	}
	r.Events = append(r.Events, e)
	return nil
}

// Snapshot returns a copy of the events so far.
func (r *Recorder) Snapshot() []harness.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]harness.Event(nil), r.Events...)
}

// WaitFor polls until pred holds over the events or d elapses.
func (r *Recorder) WaitFor(d time.Duration, pred func([]harness.Event) bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred(r.Snapshot()) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return pred(r.Snapshot())
}

// Texts returns the Message texts in order.
func Texts(evs []harness.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == harness.KindMessage {
			var m harness.Message
			if json.Unmarshal(e.Payload, &m) == nil {
				out = append(out, m.Text)
			}
		}
	}
	return out
}

// Echoes returns the texts that start with "echo:", prefix removed.
func Echoes(evs []harness.Event) []string {
	var out []string
	for _, t := range Texts(evs) {
		if strings.HasPrefix(t, "echo:") {
			out = append(out, strings.TrimPrefix(t, "echo:"))
		}
	}
	return out
}

type identity struct{}

func (identity) Redact(s string) string { return s }
