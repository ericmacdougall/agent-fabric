// Package native adapts Fabric's built-in agent loop (internal/agent) to the
// harness.Adapter interface. It does not change the loop. It wraps the loop's
// Provider and Tools with observers that report what happened as harness
// events, and maps the loop's Outcome to a harness.Final.
//
// The loop is one-shot and runs to completion, so the adapter is not
// interactive: Input returns ErrNotInteractive, Interrupt cancels the run
// (outcome interrupted, resumable through Extra {"resume":true}).
package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"fabric/internal/agent"
	"fabric/internal/harness"
)

// Name is the registry name of this adapter.
const Name = "native"

// Builder constructs the loop for a session. It must route the loop's
// Provider through o.Provider and its Tools through o.Tools; the adapter
// cannot see inside a Config it did not build.
type Builder func(spec harness.StartSpec, o *Observer) (*agent.Loop, error)

// Adapter is the native harness adapter.
type Adapter struct {
	Build  Builder
	Redact harness.Redactor // nil means harness.PatternRedactor()
}

func (a *Adapter) Name() string { return Name }

// Caps: the loop can resume from its checkpoints; it takes no input mid-run.
func (a *Adapter) Caps() harness.Caps { return harness.Caps{Resume: true} }

var (
	errInterrupted = errors.New("interrupted")
	errStopped     = errors.New("stopped")
)

type resumeSpec struct {
	Resume bool `json:"resume"`
}

// Start implements harness.Adapter.
func (a *Adapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	if a.Build == nil {
		return nil, errors.New("native: no loop builder")
	}
	var rs resumeSpec
	if len(spec.Extra) > 0 {
		if err := json.Unmarshal(spec.Extra, &rs); err != nil {
			return nil, fmt.Errorf("native: extra: %w", err)
		}
	}
	red := a.Redact
	if red == nil {
		red = harness.PatternRedactor()
	}
	em, err := harness.NewEmitter(Name, spec.SessionID, sink, red)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	o := &Observer{em: em, cancel: cancel, pending: map[string][]string{}}
	loop, err := a.Build(spec, o)
	if err != nil {
		cancel(err)
		return nil, err
	}
	s := &session{id: spec.SessionID, em: em, cancel: cancel, done: make(chan struct{})}
	go s.run(runCtx, loop, rs.Resume)
	return s, nil
}

type session struct {
	id     string
	em     *harness.Emitter
	cancel context.CancelCauseFunc
	done   chan struct{}
	final  harness.Final
}

func (s *session) ID() string { return s.id }

func (s *session) run(ctx context.Context, l *agent.Loop, resume bool) {
	defer close(s.done)
	var out agent.Outcome
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		if resume {
			out, err = l.Resume(ctx)
		} else {
			out, err = l.Run(ctx)
		}
	}()
	s.final = finalFor(out, err, context.Cause(ctx))
	_ = s.em.Final(context.Background(), s.final)
	if s.em.Err() != nil && s.final.Outcome == harness.OutcomeDone {
		s.final.Outcome = harness.OutcomeFailed
	}
}

// finalFor maps a loop outcome to a harness Final. Only a loop that ended
// done is success; budget, turn and wall limits, parks and errors are failed;
// a pending approval is stopped (resumable); a cancellation reports its cause.
func finalFor(out agent.Outcome, err error, cause error) harness.Final {
	if err == nil && out.Kind == agent.OutcomeDone {
		return harness.Final{Outcome: harness.OutcomeDone, ExitCode: -1}
	}
	switch {
	case errors.Is(cause, errInterrupted):
		return harness.Final{Outcome: harness.OutcomeInterrupted, ExitCode: -1}
	case errors.Is(cause, errStopped):
		return harness.Final{Outcome: harness.OutcomeStopped, ExitCode: -1}
	}
	if err == nil && out.Kind == agent.OutcomeApprovalRequired {
		return harness.Final{Outcome: harness.OutcomeStopped, Reason: agent.OutcomeApprovalRequired, ExitCode: -1}
	}
	reason := out.Kind
	if out.Note != "" {
		reason += ": " + out.Note
	}
	if err != nil {
		if reason != "" {
			reason += "; "
		}
		reason += err.Error()
	}
	if cause != nil && !errors.Is(cause, context.Canceled) {
		reason += "; " + cause.Error()
	}
	if reason == "" {
		reason = "loop ended without an outcome"
	}
	return harness.Final{Outcome: harness.OutcomeFailed, Reason: reason, ExitCode: -1}
}

func (s *session) Input(ctx context.Context, msg string) error { return harness.ErrNotInteractive }

func (s *session) Interrupt(ctx context.Context) error {
	s.cancel(errInterrupted)
	return nil
}

func (s *session) Stop(ctx context.Context) error {
	s.cancel(errStopped)
	_, err := s.Wait(ctx)
	return err
}

func (s *session) Wait(ctx context.Context) (harness.Final, error) {
	select {
	case <-s.done:
		return s.final, nil
	case <-ctx.Done():
		return harness.Final{}, ctx.Err()
	}
}

// Observer turns what the loop's Provider and Tools do into harness events.
type Observer struct {
	em     *harness.Emitter
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	turn    int
	pending map[string][]string // name+input digest -> tool_use ids, FIFO
}

// check ends the run if events can no longer be recorded: a run that cannot be
// recorded must not keep acting.
func (o *Observer) check() {
	if err := o.em.Err(); err != nil {
		o.cancel(fmt.Errorf("event sink failed: %w", err))
	}
}

func key(name string, input []byte) string {
	h := sha256.Sum256(input)
	return name + ":" + hex.EncodeToString(h[:])
}

// Provider wraps p.
func (o *Observer) Provider(p agent.Provider) agent.Provider { return &obsProvider{Provider: p, o: o} }

type obsProvider struct {
	agent.Provider
	o *Observer
}

func (p *obsProvider) Send(ctx context.Context, req agent.Request, h agent.Hooks) (*agent.Result, error) {
	o := p.o
	o.mu.Lock()
	o.turn++
	n := o.turn
	o.mu.Unlock()
	_ = o.em.Turn(ctx, n)
	res, err := p.Provider.Send(ctx, req, h)
	if res != nil {
		o.observe(ctx, res)
	}
	o.check()
	return res, err
}

func (o *Observer) observe(ctx context.Context, res *agent.Result) {
	for _, raw := range res.Content {
		var b struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			_ = o.em.Unknown(ctx, "undecodable content block", string(raw))
			continue
		}
		switch b.Type {
		case "text":
			_ = o.em.Message(ctx, "assistant", b.Text)
		case "tool_use":
			_ = o.em.ToolCall(ctx, b.ID, b.Name, b.Input, "requested")
			o.mu.Lock()
			k := key(b.Name, b.Input)
			o.pending[k] = append(o.pending[k], b.ID)
			o.mu.Unlock()
		case "thinking", "redacted_thinking":
			// Reasoning stays inside the harness.
		default:
			_ = o.em.Unknown(ctx, "content block type "+b.Type, string(raw))
		}
	}
	_ = o.em.Usage(ctx, harness.Usage{
		Input: res.Usage.Input, Output: res.Usage.Output,
		CacheCreate: res.Usage.CacheCreate, CacheRead: res.Usage.CacheRead,
	})
}

func (o *Observer) take(name string, input []byte) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := key(name, input)
	ids := o.pending[k]
	if len(ids) == 0 {
		return ""
	}
	o.pending[k] = ids[1:]
	return ids[0]
}

// Tools wraps ts. A DurableReviewTool stays a DurableReviewTool: the loop
// finds the review path by interface assertion, and hiding it would silently
// remove the human-review gate.
func (o *Observer) Tools(ts []agent.Tool) []agent.Tool {
	out := make([]agent.Tool, len(ts))
	for i, t := range ts {
		if rt, ok := t.(agent.DurableReviewTool); ok {
			out[i] = &obsReviewTool{obsTool: obsTool{Tool: t, o: o}, rt: rt}
		} else {
			out[i] = &obsTool{Tool: t, o: o}
		}
	}
	return out
}

type obsTool struct {
	agent.Tool
	o *Observer
}

func (t *obsTool) Run(ctx context.Context, in json.RawMessage) (string, bool) {
	out, isErr := t.Tool.Run(ctx, in)
	status := "ok"
	if isErr {
		status = "error"
	}
	_ = t.o.em.ToolCall(ctx, t.o.take(t.Name(), in), t.Name(), in, status)
	t.o.check()
	return out, isErr
}

type obsReviewTool struct {
	obsTool
	rt agent.DurableReviewTool
}

func (t *obsReviewTool) RunReviewed(ctx context.Context, in json.RawMessage) (agent.ReviewRunResult, error) {
	res, err := t.rt.RunReviewed(ctx, in)
	status := "ok"
	switch {
	case err != nil:
		status = "error"
	case res.GuardDenied:
		status = "denied"
	case res.Pause != nil:
		status = "pending_review"
	case res.IsError:
		status = "error"
	}
	id := t.o.take(t.Name(), in)
	if status == "pending_review" {
		// The call is not finished; the loop will resume it, so keep its id.
		t.o.mu.Lock()
		k := key(t.Name(), in)
		t.o.pending[k] = append([]string{id}, t.o.pending[k]...)
		t.o.mu.Unlock()
	}
	_ = t.o.em.ToolCall(ctx, id, t.Name(), in, status)
	t.o.check()
	return res, err
}
