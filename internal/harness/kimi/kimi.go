// Package kimi adapts the Kimi CLI (the user's subscription CLI; never an API
// router) to harness.Adapter. It runs one non-interactive prompt with
// `kimi --prompt=P --output-format stream-json` and maps the JSON lines to
// harness events.
//
// Kimi output is research-only and UNVERIFIED until reproduced by something
// else (feedback: Kimi is not a builder). The adapter says so in the stream
// (the first event is a system message) and in the Final reason of a
// successful run. A "done" outcome means the process exited 0 with an
// assistant reply; it is never evidence that the reply is right.
//
// The CLI is one-shot: Input returns ErrNotInteractive, Interrupt and Stop
// end the process group. Credentials stay with the CLI; this package never
// reads them, and the child gets only the environment in StartSpec.Env.
package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/proc"
	"fabric/internal/provider/claude"
)

// Name is the registry name of this adapter.
const Name = "kimi"

// UnverifiedNotice is the label every kimi session carries.
const UnverifiedNotice = "UNVERIFIED: kimi output is research-only until reproduced"

// MaxPromptBytes bounds the prompt, which travels in argv.
const MaxPromptBytes = 64 << 10

// Launcher starts the kimi process for a session.
type Launcher func(ctx context.Context, spec harness.StartSpec) (proc.Proc, error)

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\-]{0,63}$`)

// ExecLauncher runs the kimi binary directly. No sandbox is applied; a
// production caller supplies a host-profile Launcher. The prompt travels in
// argv (the CLI offers no stdin form): do not put secrets in it.
func ExecLauncher(bin string, extraArgs ...string) Launcher {
	return func(ctx context.Context, spec harness.StartSpec) (proc.Proc, error) {
		if !filepath.IsAbs(spec.Dir) {
			return nil, errors.New("kimi: Dir must be an absolute path")
		}
		if spec.Prompt == "" || len(spec.Prompt) > MaxPromptBytes {
			return nil, fmt.Errorf("kimi: prompt must be 1..%d bytes", MaxPromptBytes)
		}
		args := []string{"--prompt=" + spec.Prompt, "--output-format", "stream-json"}
		if spec.Model != "" {
			if !modelRE.MatchString(spec.Model) {
				return nil, fmt.Errorf("kimi: model %q is not an allowed model alias", spec.Model)
			}
			args = append(args, "--model="+spec.Model)
		}
		args = append(args, extraArgs...)
		cmd := exec.Command(bin, args...)
		cmd.Dir = spec.Dir
		cmd.Env = proc.EnvList(spec.Env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return proc.StartCmd(cmd)
	}
}

// Adapter is the kimi adapter.
type Adapter struct {
	Launch Launcher
	Redact harness.Redactor // nil means harness.PatternRedactor()
	// DrainGrace is how long, after the process has exited, the reader keeps
	// waiting for stdout to end before closing it. Zero means 2 seconds.
	DrainGrace time.Duration
}

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Caps() harness.Caps { return harness.Caps{} }

type session struct {
	a    *Adapter
	id   string
	em   *harness.Emitter
	proc proc.Proc

	interrupted atomic.Bool
	stopped     atomic.Bool
	replies     int // owned by the reader until done is closed

	done  chan struct{}
	final harness.Final
}

// Start implements harness.Adapter.
func (a *Adapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	if a.Launch == nil {
		return nil, errors.New("kimi: no launcher")
	}
	red := a.Redact
	if red == nil {
		red = harness.PatternRedactor()
	}
	if !filepath.IsAbs(spec.Dir) {
		return nil, errors.New("kimi: Dir must be an absolute path")
	}
	if spec.Prompt == "" || len(spec.Prompt) > MaxPromptBytes {
		return nil, fmt.Errorf("kimi: prompt must be 1..%d bytes", MaxPromptBytes)
	}
	if spec.Model != "" && !modelRE.MatchString(spec.Model) {
		return nil, fmt.Errorf("kimi: model %q is not an allowed model alias", spec.Model)
	}
	// The prompt travels in argv, which any local user can read: refuse one
	// that holds a secret-shaped string (the event stream redacts it, argv does not).
	if red.Redact(spec.Prompt) != spec.Prompt {
		return nil, errors.New("kimi: the prompt contains a secret-shaped string; it would be visible in the process list")
	}
	em, err := harness.NewEmitter(Name, spec.SessionID, sink, red)
	if err != nil {
		return nil, err
	}
	p, err := a.Launch(ctx, spec)
	if err != nil {
		return nil, err
	}
	_ = p.Stdin().Close() // one-shot: nothing is ever sent
	s := &session{a: a, id: spec.SessionID, em: em, proc: p, done: make(chan struct{})}
	_ = em.Message(context.WithoutCancel(ctx), "system", UnverifiedNotice)
	_ = em.Turn(context.WithoutCancel(ctx), 1)
	_ = em.Message(context.WithoutCancel(ctx), "user", spec.Prompt)
	go s.read()
	return s, nil
}

func (s *session) ID() string { return s.id }

func (s *session) Input(ctx context.Context, msg string) error { return harness.ErrNotInteractive }

func (s *session) Interrupt(ctx context.Context) error {
	s.interrupted.Store(true)
	return s.proc.Kill()
}

func (s *session) Stop(ctx context.Context) error {
	s.stopped.Store(true)
	_ = s.proc.Kill()
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

type wire struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	IsError    bool            `json:"is_error"`
}

type toolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
}

func (s *session) handle(line []byte) {
	ctx := context.Background()
	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		_ = s.em.Unknown(ctx, "malformed JSON line", string(line))
		return
	}
	switch w.Role {
	case "meta":
		switch w.Type {
		case "system.version", "session.resume_hint":
		default:
			_ = s.em.Unknown(ctx, "meta event type "+w.Type, string(line))
		}
	case "assistant":
		var text string
		hasText := false
		if len(w.Content) > 0 && string(w.Content) != "null" {
			if json.Unmarshal(w.Content, &text) == nil {
				hasText = true
			} else {
				_ = s.em.Unknown(ctx, "assistant content is not a string", string(line))
			}
		}
		if hasText && text != "" {
			if isHookResult(text) {
				// kimi writes hook output as an assistant-shaped line; it is not
				// a model answer, so it never satisfies "done needs a reply".
				_ = s.em.Message(ctx, "system", text)
			} else {
				s.replies++
				_ = s.em.Message(ctx, "assistant", text)
			}
		}
		for _, tc := range w.ToolCalls {
			name, args := tc.Function.Name, tc.Function.Arguments
			if name == "" {
				name = tc.Name
			}
			if len(args) == 0 {
				args = tc.Arguments
			}
			if len(args) == 0 {
				args = tc.Input
			}
			_ = s.em.ToolCall(ctx, tc.ID, name, args, "requested")
		}
	case "tool":
		status := "ok"
		if w.IsError {
			status = "error"
		}
		_ = s.em.ToolCall(ctx, w.ToolCallID, w.Name, w.Content, status)
	default:
		_ = s.em.Unknown(ctx, "unrecognised role "+w.Role, string(line))
	}
}

func (s *session) read() {
	defer close(s.done)
	out := s.proc.Stdout()
	idle := &lastRead{r: out}
	readDone := make(chan struct{})
	var panicked atomic.Value
	var busy atomic.Bool
	go func() {
		defer close(readDone)
		defer func() {
			if r := recover(); r != nil {
				panicked.Store(fmt.Sprint(r))
			}
		}()
		proc.ReadLines(idle, claude.MaxLineBytes, func(line []byte, oversize bool) {
			busy.Store(true)
			defer busy.Store(false)
			if oversize {
				_ = s.em.Unknown(context.Background(), "event exceeds the maximum line size", string(line))
			} else if len(line) > 0 {
				s.handle(line)
			}
			if s.em.Err() != nil {
				_ = s.proc.Kill() // events can no longer be recorded: stop acting
			}
		})
	}()
	select {
	case <-readDone:
	case <-s.proc.Exited():
		grace := s.a.DrainGrace
		if grace <= 0 {
			grace = 2 * time.Second
		}
		exitedAt := time.Now()
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		cut := false
	wait:
		for {
			select {
			case <-readDone:
				break wait
			case <-tk.C:
				since := exitedAt
				if l := idle.at(); l.After(since) {
					since = l
				}
				if !busy.Load() && time.Since(since) > grace || time.Since(exitedAt) > 60*time.Second {
					_ = out.Close()
					<-readDone
					cut = true
					break wait
				}
			}
		}
		if cut {
			_ = s.em.Unknown(context.Background(), "stdout cut after the process exited", "")
		}
	}
	_ = out.Close()
	if panicked.Load() != nil {
		_ = s.proc.Kill()
	}
	code, werr := s.proc.Wait()
	s.final, _ = s.em.FinalOutcome(context.Background(), s.settle(code, werr, panicked.Load()))
}

func (s *session) settle(code int, werr error, panicked any) harness.Final {
	switch {
	case panicked != nil:
		return harness.Final{Outcome: harness.OutcomeFailed, Reason: fmt.Sprintf("adapter panic: %v", panicked), ExitCode: code}
	case s.interrupted.Load():
		return harness.Final{Outcome: harness.OutcomeInterrupted, Reason: UnverifiedNotice, ExitCode: code}
	case s.stopped.Load():
		return harness.Final{Outcome: harness.OutcomeStopped, Reason: UnverifiedNotice, ExitCode: code}
	case code == 0 && werr == nil && s.replies > 0:
		return harness.Final{Outcome: harness.OutcomeDone, Reason: UnverifiedNotice, ExitCode: 0}
	}
	reason := fmt.Sprintf("exited with status %d", code)
	if code == 0 && s.replies == 0 {
		reason = "exited 0 without an assistant reply"
	}
	if werr != nil {
		reason += ": " + werr.Error()
	}
	if e := s.proc.Stderr(); e != "" {
		e = s.a.redactor().Redact(e)
		if len(e) > 700 {
			e = e[len(e)-700:]
		}
		reason += "; stderr: " + e
	}
	return harness.Final{Outcome: harness.OutcomeFailed, Reason: reason, ExitCode: code}
}

// isHookResult reports whether text looks like the CLI's hook-result block:
// a first line ending in "hook" or "hook blocked".
func isHookResult(text string) bool {
	first, _, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	return strings.HasSuffix(first, " hook") || strings.HasSuffix(first, " hook blocked")
}

func (a *Adapter) redactor() harness.Redactor {
	if a.Redact != nil {
		return a.Redact
	}
	return harness.PatternRedactor()
}

type lastRead struct {
	r    io.Reader
	last atomic.Int64
}

func (l *lastRead) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (l *lastRead) at() time.Time {
	if n := l.last.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}
