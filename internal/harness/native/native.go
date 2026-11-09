// Package native adapts Fabric's built-in agent loop (internal/agent) to the
// harness.Adapter interface. It does not change the loop. It wraps the loop's
// Provider and Tools with observers that report what happened as harness
// events, and maps the loop's Outcome to a harness.Final.
//
// The loop is one-shot and runs to completion, so the adapter is not
// interactive: Input returns ErrNotInteractive, Interrupt cancels the run
// (outcome interrupted, resumable through Extra {"resume":true}).
//
// Fail closed on the event sink: once an event cannot be recorded, the
// observer refuses to start the next provider call and refuses to run the next
// tool, so the run stops acting. It never cancels between a tool's Run and the
// loop saving that tool's result, which would leave a completed call
// unrecorded.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"fabric/internal/agent"
	"fabric/internal/harness"
)

// Name is the registry name of this adapter.
const Name = "native"

// ErrEventsUnrecorded is returned by an observed Provider or Tool once the
// event sink has failed.
var ErrEventsUnrecorded = errors.New("native: events can no longer be recorded; refusing to continue")

// Builder constructs the loop for a session. It must route the loop's
// Provider through o.Provider and its Tools through o.Tools; the adapter
// cannot see inside a Config it did not build. Use the same Redactor for the
// loop's Config.Redact and the Adapter's Redact, so one redactor covers both
// the stored transcript and the emitted events.
type Builder func(spec harness.StartSpec, o *Observer) (*agent.Loop, error)

// Adapter is the native harness adapter.
type Adapter struct {
	Build  Builder
	Redact harness.Redactor // required
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
	if a.Redact == nil {
		return nil, errors.New("native: a redactor is required")
	}
	var rs resumeSpec
	if len(spec.Extra) > 0 {
		if err := json.Unmarshal(spec.Extra, &rs); err != nil {
			return nil, fmt.Errorf("native: extra: %w", err)
		}
	}
	em, err := harness.NewEmitter(Name, spec.SessionID, sink, a.Redact)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	o := &Observer{em: em}
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
	f := finalFor(out, err, context.Cause(ctx))
	// The emitter rewrites done to failed if events were lost; Wait reports
	// what was emitted, so take the outcome from the emitter's return.
	s.final, _ = s.em.FinalOutcome(context.Background(), f)
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
		f := harness.Final{Outcome: harness.OutcomeStopped, ExitCode: -1}
		// A real failure that raced with the stop must not vanish.
		if err != nil && !errors.Is(err, context.Canceled) {
			f.Reason = err.Error()
		}
		return f
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
	em *harness.Emitter
}

// NewObserver builds an Observer that reports to em. Callers that run the loop
// themselves (internal/native, `fab harness run`) use it instead of the
// Adapter, and call Finish when the loop ends.
func NewObserver(em *harness.Emitter) *Observer { return &Observer{em: em} }

// Finish emits the session's Final for a loop outcome and returns it as
// recorded. A run is only done if the loop ended done and every event was
// recorded. stopped says the caller ended the run by cancelling its context
// (Stop), which settles as stopped rather than failed.
func (o *Observer) Finish(ctx context.Context, out agent.Outcome, err error, stopped bool) harness.Final {
	var cause error
	if stopped {
		cause = errStopped
	}
	f, _ := o.em.FinalOutcome(ctx, finalFor(out, err, cause))
	return f
}

// bg detaches observer emits from the loop's context: after Interrupt or Stop
// the last events must still be recorded.
func bg(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// Provider wraps p.
func (o *Observer) Provider(p agent.Provider) agent.Provider {
	return &obsProvider{Provider: p, o: o, turn: new(int)}
}

type obsProvider struct {
	agent.Provider
	o    *Observer
	turn *int
}

func (p *obsProvider) Send(ctx context.Context, req agent.Request, h agent.Hooks) (*agent.Result, error) {
	o := p.o
	if err := o.em.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEventsUnrecorded, err)
	}
	*p.turn++
	if err := o.em.Turn(bg(ctx), *p.turn); err != nil {
		// The turn is not recorded: do not pay for a call whose result
		// would be thrown away.
		return nil, fmt.Errorf("%w: %v", ErrEventsUnrecorded, err)
	}
	res, err := p.Provider.Send(ctx, req, h)
	if res != nil {
		o.observe(bg(ctx), res)
	}
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

// callID is the durable id the loop attached to this call, or empty (a resumed
// or directly invoked tool).
func callID(ctx context.Context) string {
	if c, ok := agent.ToolCallFromContext(ctx); ok {
		return c.ID
	}
	return ""
}

type obsTool struct {
	agent.Tool
	o *Observer
}

func (t *obsTool) Run(ctx context.Context, in json.RawMessage) (string, bool) {
	if err := t.o.em.Err(); err != nil {
		// Not run, and said so: the loop records this as the call's result.
		return "not run: " + ErrEventsUnrecorded.Error(), true
	}
	out, isErr := t.Tool.Run(ctx, in)
	status := "ok"
	if isErr {
		status = "error"
	}
	// No cancellation here: the result must reach the loop's checkpoint.
	_ = t.o.em.ToolCall(bg(ctx), callID(ctx), t.Name(), in, status)
	return out, isErr
}

type obsReviewTool struct {
	obsTool
	rt agent.DurableReviewTool
}

func (t *obsReviewTool) RunReviewed(ctx context.Context, in json.RawMessage) (agent.ReviewRunResult, error) {
	if err := t.o.em.Err(); err != nil {
		// Nothing was dispatched; the call stays pending and resumable.
		return agent.ReviewRunResult{}, fmt.Errorf("%w: %v", ErrEventsUnrecorded, err)
	}
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
	_ = t.o.em.ToolCall(bg(ctx), callID(ctx), t.Name(), in, status)
	return res, err
}
