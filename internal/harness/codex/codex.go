// Package codex adapts the Codex app-server protocol (newline-delimited
// JSON-RPC over the stdio of `codex app-server`) to harness.Adapter.
//
// It is ported from the agent-harness console's Codex client: the request and
// notification shapes, the turn-identity rules (a notification belongs to a
// turn only when its turnId is present, unambiguous and equal to the turn
// bound by the turn/start reply) and the buffering bounds. The console's own
// event log and UI are not ported; everything leaves through harness.Emitter.
//
// Lifecycle. One session is one app-server process and one Codex thread. The
// prompt and every Input become turns, queued and sent in order, one at a
// time. The session ends with:
//
//   - done when a turn completes and nothing is queued (unless keep_open);
//   - failed when a turn fails, the process exits without a result, a request
//     is rejected or times out, output limits are exceeded or events cannot be
//     recorded;
//   - stopped when Stop is called.
//
// Interrupt sends turn/interrupt for the running turn and keeps the session:
// an interrupted session waits for the next Input or for Stop, whatever
// keep_open says. With keep_open the session also stays after a completed
// turn, as a chat.
//
// The adapter never grants approvals. With approvalPolicy "never" and a
// workspace-write sandbox (the defaults) Codex should not ask; if it does,
// the request is declined and reported as a denied tool call.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"fabric/internal/harness"
)

// Name is the registry name of this adapter.
const Name = "codex"

// Defaults and bounds.
const (
	// MaxLineBytes bounds one protocol line (same bound as the Claude stream).
	MaxLineBytes = 4 << 20
	// MaxTotalBytes bounds all stdout a session may produce.
	MaxTotalBytes = 256 << 20
	// MaxInputBytes bounds one Input message.
	MaxInputBytes = 1 << 20
	// MaxQueuedInputs bounds inputs waiting for their turn.
	MaxQueuedInputs = 1024

	maxUnknownEvents = 256
	defaultReqTimout = 60 * time.Second
	defaultKillGrace = 2 * time.Second
	writeDeadline    = 10 * time.Second
)

// Adapter is the Codex harness adapter. The zero value is usable and runs
// `codex app-server` with an empty environment.
type Adapter struct {
	// Control enables failclosed file-edit-only preflight.
	Control *Control

	// Command is the argv that speaks the app-server protocol on stdio.
	// Default: codex app-server. Resolved with the caller's PATH; the child
	// itself gets only Env.
	Command []string
	// BaseEnv is merged under StartSpec.Env (StartSpec wins). Together they
	// are the complete environment of the process.
	BaseEnv map[string]string
	// Redact scrubs every string leaving the adapter. Default
	// harness.PatternRedactor.
	Redact harness.Redactor
	// KeepOpen keeps a session open after a completed turn (chat mode).
	// StartSpec.Extra {"keep_open":...} overrides it.
	KeepOpen bool
	// MaxLine and MaxTotal override MaxLineBytes and MaxTotalBytes.
	MaxLine  int
	MaxTotal int64
	// RequestTimeout bounds initialize, thread and turn/start replies.
	RequestTimeout time.Duration
	// KillGrace is how long a stopping process gets after SIGTERM before
	// SIGKILL.
	KillGrace time.Duration
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return Name }

// Caps implements harness.Adapter. The protocol exposes child threads, but
// the event model has no place for them, so subagents are not claimed: their
// events are reported as unknown rather than merged into this session.
func (a *Adapter) Caps() harness.Caps {
	return harness.Caps{Resume: true, Interactive: true, Subagents: false}
}

// Extra is the adapter-specific part of StartSpec.Extra.
type Extra struct {
	// ResumeThread continues an existing Codex thread.
	ResumeThread string `json:"resume_thread,omitempty"`
	// Sandbox is read-only or workspace-write (the default). Full access is
	// not selectable through this adapter.
	Sandbox string `json:"sandbox,omitempty"`
	// ApprovalPolicy is never (the default), untrusted or on-request. The
	// adapter declines every approval request either way.
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	// KeepOpen overrides Adapter.KeepOpen.
	KeepOpen *bool `json:"keep_open,omitempty"`
}

type options struct {
	resume   string
	sandbox  string
	approval string
	keepOpen bool
}

func (a *Adapter) options(spec harness.StartSpec) (options, error) {
	o := options{sandbox: "workspace-write", approval: "never", keepOpen: a.KeepOpen}
	if len(spec.Extra) > 0 {
		var x Extra
		d := json.NewDecoder(bytes.NewReader(spec.Extra))
		d.DisallowUnknownFields()
		if err := d.Decode(&x); err != nil {
			return o, fmt.Errorf("codex: extra: %w", err)
		}
		if x.Sandbox != "" {
			o.sandbox = x.Sandbox
		}
		if x.ApprovalPolicy != "" {
			o.approval = x.ApprovalPolicy
		}
		if x.KeepOpen != nil {
			o.keepOpen = *x.KeepOpen
		}
		o.resume = x.ResumeThread
	}
	switch o.sandbox {
	case "read-only", "workspace-write":
	default:
		return o, fmt.Errorf("codex: sandbox %q not allowed (read-only or workspace-write)", o.sandbox)
	}
	switch o.approval {
	case "never", "untrusted", "on-request":
	default:
		return o, fmt.Errorf("codex: approval_policy %q not allowed", o.approval)
	}
	return o, nil
}

// environ builds the process environment from the allow-list only.
func (a *Adapter) environ(spec harness.StartSpec) ([]string, error) {
	m := map[string]string{}
	for k, v := range a.BaseEnv {
		m[k] = v
	}
	for k, v := range spec.Env {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("codex: invalid environment entry %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

// Start implements harness.Adapter.
func (a *Adapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	var control *Control
	if a.Control != nil {
		c := *a.Control
		c.InstructionSources = append([]string(nil), c.InstructionSources...)
		control = &c
		if !control.PreflightOnly && (spec.Prompt != DemoPrompt || control.CatalogOnly) {
			return nil, errors.New("codex: controlled task mismatch")
		}
		if err := control.validate(); err != nil {
			return nil, err
		}
		if err := control.checkProxyEnv(a.BaseEnv); err != nil {
			return nil, err
		}
		if len(spec.Extra) > 0 || a.KeepOpen || spec.Model != "" {
			return nil, errors.New("codex: controlled mode cannot resume or override options")
		}
		if len(spec.Env) > 0 {
			return nil, errors.New("codex: controlled environment rejected")
		}
		for _, env := range []map[string]string{a.BaseEnv} {
			for k, v := range env {
				switch k {
				case "HOME", "LANG", "LC_ALL":
				case "PATH":
					if v != "/usr/bin:/bin" {
						return nil, errors.New("codex: controlled path rejected")
					}
				case "SHELL":
					if v != "/bin/sh" {
						return nil, errors.New("codex: controlled shell rejected")
					}
				case "RUST_LOG":
					if v != "off" {
						return nil, errors.New("codex: diagnostic trace override refused")
					}
				case "PYTHONNOUSERSITE", "PYTHONDONTWRITEBYTECODE":
					if v != "1" {
						return nil, errors.New("codex: controlled Python startup rejected")
					}
				case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
					if control.ProxyURL == "" || v != control.ProxyURL {
						return nil, errors.New("codex: owned proxy mismatch")
					}
				case "NO_PROXY", "no_proxy":
					if control.ProxyURL == "" || v != "" {
						return nil, errors.New("codex: proxy bypass refused")
					}
				default:
					return nil, errors.New("codex: controlled environment rejected")
				}
			}
		}
	}
	o, err := a.options(spec)
	if err != nil {
		return nil, err
	}
	if spec.Dir == "" {
		return nil, errors.New("codex: StartSpec.Dir is required")
	}
	if spec.Prompt == "" && !o.keepOpen && o.resume == "" && !(control != nil && control.PreflightOnly) {
		return nil, errors.New("codex: empty prompt for a session that is not keep_open")
	}
	if len(spec.Prompt) > MaxInputBytes {
		return nil, errors.New("codex: prompt too large")
	}
	env, err := a.environ(spec)
	if err != nil {
		return nil, err
	}
	red := a.Redact
	if red == nil {
		red = harness.PatternRedactor()
	}
	em, err := harness.NewEmitter(Name, spec.SessionID, sink, red)
	if err != nil {
		return nil, err
	}
	argv := a.Command
	if len(argv) == 0 {
		argv = []string{"codex", "app-server"}
	}
	s := &session{
		id: spec.SessionID, em: em, opts: o, spec: spec, control: control,
		maxLine: orInt(a.MaxLine, MaxLineBytes), maxTotal: orInt64(a.MaxTotal, MaxTotalBytes),
		reqTimeout: orDur(a.RequestTimeout, defaultReqTimout), grace: orDur(a.KillGrace, defaultKillGrace),
		in: make(chan any, 64), quit: make(chan struct{}), done: make(chan struct{}),
		readerDone: make(chan struct{}), exited: make(chan struct{}),
		pend: map[uint64]string{}, completed: map[string]bool{}, pendingHooks: map[string]bool{},
		ctx: context.WithoutCancel(ctx),
	}
	if spec.Prompt != "" {
		s.queue = append(s.queue, spec.Prompt)
	}
	if control != nil && s.maxLine > RequirementsMaxBytes {
		s.maxLine = RequirementsMaxBytes
	}
	p, err := spawn(argv, env, spec.Dir, &cappedWriter{})
	if err != nil {
		return nil, err
	}
	s.p = p
	s.wg.Add(2)
	go s.readLoop()
	go s.observeLoop()
	go s.run()
	return s, nil
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
func orInt64(v, d int64) int64 {
	if v > 0 {
		return v
	}
	return d
}
func orDur(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

// session implements harness.Session. All protocol state lives in the run
// goroutine (the actor); other goroutines only post events to it.
type session struct {
	control           *Control
	pendingHooks      map[string]bool
	hooksObserved     bool
	requirementsPhase bool
	requirementsID    string
	id                string
	em                *harness.Emitter
	opts              options
	spec              harness.StartSpec
	ctx               context.Context
	p                 *proc

	maxLine    int
	maxTotal   int64
	reqTimeout time.Duration
	grace      time.Duration

	in         chan any
	quit       chan struct{} // closed when the session is finishing
	done       chan struct{} // closed when everything has stopped
	readerDone chan struct{} // closed when stdout reached EOF or the reader stopped
	exited     chan struct{} // closed when the process has been reaped
	wg         sync.WaitGroup

	mu     sync.Mutex
	thread string // exposed through ThreadID
	final  harness.Final

	// actor state
	phase      int
	nextReq    uint64
	pend       map[uint64]string
	queue      []string
	cur        *turn
	turnIdx    int
	completed  map[string]bool
	unknownN   int
	suppressed int
	finished   bool
	timer      *time.Timer
	timerC     <-chan time.Time
	timerKind  string
}

// Events posted to the actor.
type (
	lineEv  struct{ l line }
	limitEv struct{}
	exitEv  struct {
		code int
		err  error
	}
	inputCmd struct {
		msg   string
		reply chan error
	}
	interruptCmd struct{ reply chan error }
	stopCmd      struct{ reply chan error }
)

func (s *session) post(ev any) bool {
	select {
	case s.in <- ev:
		return true
	case <-s.quit:
		return false
	}
}

// ID implements harness.Session.
func (s *session) ID() string { return s.id }

// ThreadID is the Codex thread of this session once it exists, for callers
// that want to resume it later (StartSpec.Extra resume_thread).
func (s *session) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread
}

func (s *session) ended() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// command posts c and waits for the actor's reply, or for the end of the
// session.
func (s *session) command(ctx context.Context, c any, reply chan error) error {
	if s.ended() {
		return harness.ErrSessionEnded
	}
	select {
	case s.in <- c:
	case <-s.done:
		return harness.ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		select {
		case err := <-reply:
			return err
		default:
			return harness.ErrSessionEnded
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Input implements harness.Session: msg becomes the next user turn, queued in
// order behind the turns already waiting.
func (s *session) Input(ctx context.Context, msg string) error {
	if s.control != nil {
		return harness.ErrNotInteractive
	}
	if len(msg) > MaxInputBytes {
		return errors.New("codex: input too large")
	}
	r := make(chan error, 1)
	return s.command(ctx, inputCmd{msg: msg, reply: r}, r)
}

// Interrupt implements harness.Session.
func (s *session) Interrupt(ctx context.Context) error {
	r := make(chan error, 1)
	err := s.command(ctx, interruptCmd{reply: r}, r)
	if errors.Is(err, harness.ErrSessionEnded) {
		return nil // nothing left to interrupt
	}
	return err
}

// Stop implements harness.Session.
func (s *session) Stop(ctx context.Context) error {
	r := make(chan error, 1)
	err := s.command(ctx, stopCmd{reply: r}, r)
	if err != nil && !errors.Is(err, harness.ErrSessionEnded) {
		return err
	}
	_, err = s.Wait(ctx)
	return err
}

// Wait implements harness.Session.
func (s *session) Wait(ctx context.Context) (harness.Final, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.final, nil
	case <-ctx.Done():
		return harness.Final{}, ctx.Err()
	}
}
