// Package claudecode adapts an interactive Claude Code process to
// harness.Adapter. Unlike the one-shot `claude -p` launch in
// internal/provider/claude, the process stays up and reads user turns as
// stream-json lines on stdin (`--input-format stream-json`), so Input
// delivers the next turn, Interrupt sends a control request that aborts the
// current turn without ending the session, and Stop closes stdin and lets the
// process drain.
//
// The adapter reads only the process's stdout. It never gives the process
// anything but the environment in StartSpec.Env, and it does not sandbox:
// production wiring supplies a Launcher that starts the process under a host
// profile (internal/host).
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"fabric/internal/harness"
	"fabric/internal/provider/claude"
)

// Name is the registry name of this adapter.
const Name = "claude-code"

// Adapter is the interactive Claude Code adapter.
type Adapter struct {
	Launch Launcher
	Redact harness.Redactor // nil means harness.PatternRedactor()
	// SingleTurn closes stdin after the first result, so the process exits and
	// the session ends done. Without it the session lives until Stop.
	SingleTurn bool
	// StopGrace is how long Stop waits for the process to drain before it is
	// killed. Zero means 10 seconds.
	StopGrace time.Duration
}

func (a *Adapter) Name() string { return Name }

// Caps: interactive; resume is not wired yet.
func (a *Adapter) Caps() harness.Caps { return harness.Caps{Interactive: true} }

type session struct {
	a    *Adapter
	id   string
	em   *harness.Emitter
	proc Proc

	wmu      sync.Mutex // serialises stdin writes and counters below
	turn     int
	sent     int
	ctlSeq   int
	inClosed bool
	stopping atomic.Bool

	stopOnce sync.Once

	// owned by the reader goroutine until done is closed
	results   int
	lastOK    bool
	lastSub   string
	tools     map[string]toolInfo
	endedSelf bool

	done  chan struct{}
	final harness.Final
}

type toolInfo struct {
	name  string
	input []byte
}

// Start implements harness.Adapter.
func (a *Adapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	if a.Launch == nil {
		return nil, errors.New("claudecode: no launcher")
	}
	red := a.Redact
	if red == nil {
		red = harness.PatternRedactor()
	}
	em, err := harness.NewEmitter(Name, spec.SessionID, sink, red)
	if err != nil {
		return nil, err
	}
	proc, err := a.Launch(ctx, spec)
	if err != nil {
		return nil, err
	}
	s := &session{a: a, id: spec.SessionID, em: em, proc: proc, tools: map[string]toolInfo{}, done: make(chan struct{})}
	go s.read()
	if spec.Prompt != "" {
		if err := s.send(context.WithoutCancel(ctx), spec.Prompt); err != nil {
			_ = proc.Kill()
			<-s.done
			return nil, fmt.Errorf("claudecode: send prompt: %w", err)
		}
	}
	return s, nil
}

func (s *session) ID() string { return s.id }

func (s *session) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.proc.Stdin().Write(append(b, '\n'))
	return err
}

// send records and writes one user turn. The Turn event precedes the write so
// every reply the process produces follows it in the stream.
func (s *session) send(ctx context.Context, text string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.stopping.Load() || s.inClosed {
		return harness.ErrSessionEnded
	}
	select {
	case <-s.done:
		return harness.ErrSessionEnded
	default:
	}
	s.turn++
	_ = s.em.Turn(ctx, s.turn)
	_ = s.em.Message(ctx, "user", text)
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	if err := s.writeLine(msg); err != nil {
		return fmt.Errorf("%w: %v", harness.ErrSessionEnded, err)
	}
	s.sent++
	return nil
}

func (s *session) Input(ctx context.Context, msg string) error { return s.send(ctx, msg) }

func (s *session) Interrupt(ctx context.Context) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return nil
	default:
	}
	if s.inClosed {
		return nil
	}
	s.ctlSeq++
	req := map[string]any{"type": "control_request", "request_id": fmt.Sprintf("fab-%d", s.ctlSeq),
		"request": map[string]any{"subtype": "interrupt"}}
	if err := s.writeLine(req); err != nil {
		return nil // the process is already gone; Interrupt on an ended session is a no-op
	}
	return nil
}

func (s *session) closeInput() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if !s.inClosed {
		s.inClosed = true
		_ = s.proc.Stdin().Close()
	}
}

func (s *session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		// Set the flag and arm the timeout before touching stdin: a write
		// stuck on a process that stopped reading holds wmu, and only ending
		// the process frees it.
		s.stopping.Store(true)
		grace := s.a.StopGrace
		if grace <= 0 {
			grace = 10 * time.Second
		}
		go func() {
			select {
			case <-s.done:
			case <-time.After(grace):
				_ = s.proc.Kill()
			}
		}()
		s.closeInput()
	})
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

// read consumes stdout until EOF, then settles the session.
func (s *session) read() {
	defer close(s.done)
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		readLines(s.proc.Stdout(), claude.MaxLineBytes, func(line []byte, oversize bool) {
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
	if panicked != nil {
		_ = s.proc.Kill()
	}
	code, werr := s.proc.Wait()
	s.wmu.Lock()
	pending := s.sent - s.results
	s.wmu.Unlock()
	stopping := s.stopping.Load()
	f := s.settle(code, werr, stopping, pending, panicked)
	s.final, _ = s.em.FinalOutcome(context.Background(), f)
}

func (s *session) settle(code int, werr error, stopping bool, pending int, panicked any) harness.Final {
	switch {
	case panicked != nil:
		return harness.Final{Outcome: harness.OutcomeFailed, Reason: fmt.Sprintf("adapter panic: %v", panicked), ExitCode: code}
	case !s.endedSelf && stopping:
		f := harness.Final{Outcome: harness.OutcomeStopped, ExitCode: code}
		if pending > 0 {
			f.Reason = fmt.Sprintf("undelivered_inputs=%d", pending)
		}
		return f
	case code == 0 && werr == nil && s.results > 0 && s.lastOK:
		return harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0}
	}
	reason := fmt.Sprintf("exited with status %d", code)
	switch {
	case s.results == 0:
		reason = fmt.Sprintf("exited without a result (status %d)", code)
	case !s.lastOK:
		reason = fmt.Sprintf("last turn did not succeed (%s), status %d", s.lastSub, code)
	}
	if werr != nil {
		reason += ": " + werr.Error()
	}
	if e := s.proc.Stderr(); e != "" {
		reason += "; stderr: " + e
	}
	return harness.Final{Outcome: harness.OutcomeFailed, Reason: reason, ExitCode: code}
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

type wire struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Message *struct {
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
			CC  int64 `json:"cache_creation_input_tokens"`
			CR  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (s *session) handle(line []byte) {
	ctx := context.Background()
	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		_ = s.em.Unknown(ctx, "malformed JSON line", string(line))
		return
	}
	switch w.Type {
	case "system":
		switch w.Subtype {
		case "init", "api_retry", "status", "compact_boundary":
		default:
			_ = s.em.Unknown(ctx, "system event subtype "+w.Subtype, string(line))
		}
	case "assistant":
		if w.Message == nil {
			_ = s.em.Unknown(ctx, "assistant event without a message", string(line))
			return
		}
		for _, b := range s.blocks(w.Message.Content, line) {
			switch b.Type {
			case "text":
				_ = s.em.Message(ctx, "assistant", b.Text)
			case "tool_use":
				s.tools[b.ID] = toolInfo{name: b.Name, input: b.Input}
				_ = s.em.ToolCall(ctx, b.ID, b.Name, b.Input, "requested")
			case "thinking", "redacted_thinking":
			default:
				_ = s.em.Unknown(ctx, "assistant block type "+b.Type, string(line))
			}
		}
		if u := w.Message.Usage; u != nil {
			_ = s.em.Usage(ctx, harness.Usage{Input: u.In, Output: u.Out, CacheCreate: u.CC, CacheRead: u.CR})
		}
	case "user":
		if w.Message == nil {
			return
		}
		for _, b := range s.blocks(w.Message.Content, line) {
			if b.Type != "tool_result" {
				continue
			}
			ti := s.tools[b.ToolUseID]
			status := "ok"
			if b.IsError {
				status = "error"
			}
			_ = s.em.ToolCall(ctx, b.ToolUseID, ti.name, ti.input, status)
		}
	case "result":
		s.wmu.Lock()
		s.results++
		s.lastOK = w.Subtype == "success" && !w.IsError
		s.lastSub = w.Subtype
		s.wmu.Unlock()
		if !s.lastOK {
			_ = s.em.Message(ctx, "system", "turn ended: "+w.Subtype)
		}
		if s.a.SingleTurn {
			s.endedSelf = true
			s.closeInput()
		}
	case "rate_limit_event", "control_response", "stream_event":
	default:
		_ = s.em.Unknown(ctx, "unrecognised event type "+w.Type, string(line))
	}
}

// blocks decodes message content, which Claude Code sends either as a string
// or as an array of blocks. Anything else is reported as unknown.
func (s *session) blocks(raw json.RawMessage, line []byte) []block {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var t string
		if json.Unmarshal(raw, &t) == nil {
			return []block{{Type: "text", Text: t}}
		}
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		_ = s.em.Unknown(context.Background(), "undecodable message content", string(line))
		return nil
	}
	return bs
}

// readLines calls f for every line of r, with the line truncated to max and
// oversize set when it was longer. A line longer than max is skipped to its
// end, not allowed to grow memory.
func readLines(r io.Reader, max int, f func(line []byte, oversize bool)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var cur []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !over {
			if len(cur)+len(chunk) > max {
				over = true
				cur = append(cur, chunk[:max-len(cur)]...)
			} else {
				cur = append(cur, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(chunk) > 0 || len(cur) > 0 {
			line := cur
			for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			f(line, over)
		}
		cur, over = nil, false
		if err != nil {
			return
		}
	}
}
