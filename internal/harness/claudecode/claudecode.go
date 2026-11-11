// Package claudecode adapts an interactive Claude Code process to
// harness.Adapter. Unlike the one-shot `claude -p` launch in
// internal/provider/claude, the process stays up and reads user turns as
// stream-json lines on stdin (`--input-format stream-json`), so Input
// delivers the next turn, Interrupt sends a control request that aborts the
// current turn without ending the session, and Stop closes stdin and lets the
// process drain.
//
// Concurrency design (goodqa#15373 red-team): stdout is drained by one reader
// goroutine that never takes a lock a writer can hold; stdin is written by one
// writer goroutine fed by a bounded queue, so Input and Interrupt return when
// their context ends even if the child has stopped reading. A leader that has
// exited has its whole process group killed; a descendant that left the group
// and still holds stdout is cut off after a short drain deadline.
//
// The adapter reads only the process's stdout and gives it nothing but the
// environment in StartSpec.Env. It does not sandbox: production wiring
// supplies a Launcher that starts the process under a host profile
// (internal/host). A "done" outcome means the process reported success and
// exited 0; it is not verification of the work.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/proc"
	"fabric/internal/provider/claude"
)

// Name is the registry name of this adapter.
const Name = "claude-code"

// Adapter is the interactive Claude Code adapter.
type Adapter struct {
	Launch Launcher
	// FixedWrite enables the narrow callback and lifecycle-only event reducer.
	// Native tool/settings/OS enforcement is the launcher's responsibility.
	FixedWrite *FixedWrite
	Redact     harness.Redactor // nil means harness.PatternRedactor()
	// SingleTurn closes stdin after the first result, so the process exits and
	// the session ends done. Without it the session lives until Stop.
	SingleTurn bool
	// StopGrace is how long Stop waits for the process to drain before it is
	// killed. Zero means 10 seconds.
	StopGrace time.Duration
	// DrainGrace is how long, after the leader has exited, the reader keeps
	// waiting for stdout to reach EOF before closing it. Zero means 2 seconds.
	DrainGrace time.Duration
	// DrainHardCap bounds the whole drain after the leader exits, even if the
	// pipe keeps producing. Zero means 60 seconds.
	DrainHardCap time.Duration
}

// idleReader records when data was last read.
type idleReader struct {
	r    io.Reader
	last atomic.Int64
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (i *idleReader) lastRead() time.Time {
	if n := i.last.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (a *Adapter) redactor() harness.Redactor {
	if a.Redact != nil {
		return a.Redact
	}
	return harness.PatternRedactor()
}

func (a *Adapter) Name() string { return Name }

// Caps: interactive; resume is not wired yet.
func (a *Adapter) Caps() harness.Caps { return harness.Caps{Interactive: true} }

const queueSize = 64

type session struct {
	a    *Adapter
	id   string
	em   *harness.Emitter
	proc Proc

	// sendSem orders concurrent Inputs; slots bounds queued user messages.
	// Neither is touched by the reader.
	sendSem  chan struct{}
	slots    chan struct{}
	settling bool // set under mu once the final accounting is read
	// mu guards the counters and flags below. Held only for short, non-blocking
	// sections; never across a write, a queue operation or an emit.
	mu       sync.Mutex
	turn     int
	sent     int // inputs queued since the last result arrived
	total    int // every input accepted
	results  int
	lastOK   bool
	lastSub  string
	ctlSeq   int
	inClosed bool

	stopping atomic.Bool
	stopOnce sync.Once
	wq       chan qmsg
	closeIn  chan struct{}

	// owned by the reader goroutine until done is closed
	tools      map[string]toolInfo
	endedSelf  bool
	fixedInit  bool
	fixedBytes int
	fixedHooks *fixedHookState

	done  chan struct{}
	final harness.Final
}

// qmsg is one queued stdin line. slot is true for user messages, which hold a
// queue reservation released once the line is written.
type qmsg struct {
	b    []byte
	slot bool
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
	if a.FixedWrite != nil && (!a.SingleTurn || !cleanAbsentArtifact(a.FixedWrite.Path)) {
		return nil, errors.New("claudecode: invalid fixed write job")
	}
	var fixedHooks *fixedHookState
	if a.FixedWrite != nil {
		var err error
		fixedHooks, err = newFixedHookState(a.FixedWrite.RequiredHooks)
		if err != nil {
			return nil, err
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
	proc, err := a.Launch(ctx, spec)
	if err != nil {
		return nil, err
	}
	s := &session{a: a, id: spec.SessionID, em: em, proc: proc, tools: map[string]toolInfo{}, done: make(chan struct{}),
		wq: make(chan qmsg, queueSize+8), closeIn: make(chan struct{}),
		sendSem: make(chan struct{}, 1), slots: make(chan struct{}, queueSize)}
	s.fixedHooks = fixedHooks
	go s.write()
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

// Results is how many results the child has produced so far. A caller that
// sends one input at a time and waits for the matching result can tell a lost
// input from a merged one, which a stream of queued inputs cannot.
func (s *session) Results() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results
}

// write is the only goroutine that writes stdin. On close it flushes what is
// queued, then closes stdin. A write error ends writing; the child exiting
// settles the session.
func (s *session) write() {
	in := s.proc.Stdin()
	defer in.Close()
	put := func(m qmsg) bool {
		_, err := in.Write(m.b)
		if m.slot {
			<-s.slots
		}
		return err == nil
	}
	for {
		select {
		case m := <-s.wq:
			if !put(m) {
				return
			}
		case <-s.closeIn:
			for {
				select {
				case m := <-s.wq:
					if !put(m) {
						return
					}
				default:
					return
				}
			}
		case <-s.done:
			return
		}
	}
}

func (s *session) enqueue(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case s.wq <- qmsg{b: append(b, '\n')}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return harness.ErrSessionEnded
	}
}

func (s *session) ended() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// send records and queues one user turn. The Turn event precedes the queueing
// so every reply the process produces follows it in the stream.
func (s *session) send(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err // a cancelled Input is refused before anything is recorded
	}
	// Order concurrent Inputs, but give up when ctx ends.
	select {
	case s.sendSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return harness.ErrSessionEnded
	}
	defer func() { <-s.sendSem }()
	// Reserve queue room first: an input that is refused (ctx, full queue,
	// ended) leaves no turn record and no count.
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return harness.ErrSessionEnded
	}
	s.mu.Lock()
	if s.stopping.Load() || s.inClosed || s.settling || s.ended() {
		s.mu.Unlock()
		<-s.slots
		return harness.ErrSessionEnded
	}
	s.turn++
	s.sent++
	s.total++
	n := s.turn
	s.mu.Unlock()
	// Recording must not depend on the caller's ctx: a cancelled Input would
	// otherwise make the sink fail and end the session.
	rctx := context.WithoutCancel(ctx)
	_ = s.em.Turn(rctx, n)
	if s.a.FixedWrite == nil {
		_ = s.em.Message(rctx, "user", text)
	}
	b, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if err == nil {
		select {
		case s.wq <- qmsg{b: append(b, '\n'), slot: true}: // room is reserved, so this does not block
			return nil
		default:
			err = errors.New("claudecode: input queue unexpectedly full")
		}
	}
	s.mu.Lock()
	s.sent-- // not queued: not counted as undelivered
	s.total--
	s.mu.Unlock()
	<-s.slots
	return err
}

func (s *session) Input(ctx context.Context, msg string) error { return s.send(ctx, msg) }

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	if s.inClosed || s.ended() {
		s.mu.Unlock()
		return nil
	}
	s.ctlSeq++
	id := fmt.Sprintf("fab-%d", s.ctlSeq)
	s.mu.Unlock()
	err := s.enqueue(ctx, map[string]any{"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": "interrupt"}})
	if errors.Is(err, harness.ErrSessionEnded) {
		return nil // Interrupt on an ended session is a no-op
	}
	return err
}

func (s *session) closeInput() {
	s.mu.Lock()
	first := !s.inClosed
	s.inClosed = true
	s.mu.Unlock()
	if first {
		close(s.closeIn)
	}
}

func (s *session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
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

// read consumes stdout until EOF (or the drain deadline), then settles.
func (s *session) read() {
	defer close(s.done)
	var panicked atomic.Value
	readDone := make(chan struct{})
	out := s.proc.Stdout()
	idle := &idleReader{r: out}
	var busy atomic.Bool
	go func() {
		defer close(readDone)
		defer func() {
			if r := recover(); r != nil {
				panicked.Store(fmt.Sprint(r))
			}
		}()
		readLines(idle, claude.MaxLineBytes, func(line []byte, oversize bool) {
			busy.Store(true)
			defer busy.Store(false)
			if oversize {
				if s.a.FixedWrite != nil {
					s.a.FixedWrite.refuse()
					_ = s.proc.Kill()
				} else {
					_ = s.em.Unknown(context.Background(), "event exceeds the maximum line size", string(line))
				}
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
		// The leader is gone and its group is dead. A descendant that left the
		// group may still hold stdout open. Cut the pipe only when it has been
		// idle for DrainGrace and the reader is not busy in the sink, or after
		// a hard cap; a cut is recorded as an unknown event, never silent.
		grace := s.a.DrainGrace
		if grace <= 0 {
			grace = 2 * time.Second
		}
		hard := s.a.DrainHardCap
		if hard <= 0 {
			hard = 60 * time.Second
		}
		exitedAt := time.Now()
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		cut := ""
	wait:
		for {
			select {
			case <-readDone:
				break wait
			case <-tk.C:
				since := exitedAt
				if l := idle.lastRead(); l.After(since) {
					since = l
				}
				switch {
				case time.Since(exitedAt) > hard:
					cut = "hard drain cap reached"
				case !busy.Load() && time.Since(since) > grace:
					cut = "stdout idle after the process exited (a descendant outside the process group holds it open)"
				}
				if cut != "" {
					_ = out.Close()
					<-readDone
					break wait
				}
			}
		}
		if cut != "" {
			if s.a.FixedWrite != nil {
				s.a.FixedWrite.refuse()
			} else {
				_ = s.em.Unknown(context.Background(), "stdout cut: "+cut, "")
			}
		}
	}
	_ = out.Close()
	if panicked.Load() != nil {
		_ = s.proc.Kill()
	}
	code, werr := s.proc.Wait()
	s.mu.Lock()
	s.settling = true
	pending := s.sent
	results, lastOK, lastSub, total := s.results, s.lastOK, s.lastSub, s.total
	s.mu.Unlock()
	f := s.settle(code, werr, s.stopping.Load(), pending, results, lastOK, lastSub, panicked.Load())
	if (f.Outcome == harness.OutcomeDone || f.Outcome == harness.OutcomeStopped) && results > 0 && total > results {
		// Real claude folds inputs queued during a turn into that turn, so
		// fewer results than inputs is normal; but a lost input looks the
		// same. Say so instead of claiming either.
		if f.Reason != "" {
			f.Reason += "; "
		}
		f.Reason += fmt.Sprintf("inputs=%d results=%d (queued inputs may have been merged by the harness, or lost)", total, results)
	}
	s.final, _ = s.em.FinalOutcome(context.Background(), f)
}

func (s *session) settle(code int, werr error, stopping bool, pending, results int, lastOK bool, lastSub string, panicked any) harness.Final {
	if s.a.FixedWrite != nil {
		if code == 0 && werr == nil && results == 1 && lastOK && pending <= 0 && panicked == nil && s.a.FixedWrite.Approved() && s.fixedHooks.complete() {
			return harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0}
		}
		return harness.Final{Outcome: harness.OutcomeFailed, Reason: "controlled job failed or cleanup uncertain", ExitCode: code}
	}
	switch {
	case panicked != nil:
		return harness.Final{Outcome: harness.OutcomeFailed, Reason: fmt.Sprintf("adapter panic: %v", panicked), ExitCode: code}
	case !s.endedSelf && stopping:
		f := harness.Final{Outcome: harness.OutcomeStopped, ExitCode: code}
		if pending > 0 {
			f.Reason = fmt.Sprintf("undelivered_inputs=%d", pending)
		}
		return f
	case code == 0 && werr == nil && results > 0 && lastOK && pending <= 0:
		return harness.Final{Outcome: harness.OutcomeDone, ExitCode: 0}
	}
	reason := fmt.Sprintf("exited with status %d", code)
	switch {
	case results == 0:
		reason = fmt.Sprintf("exited without a result (status %d)", code)
	case !lastOK:
		reason = fmt.Sprintf("last turn did not succeed (%s), status %d", lastSub, code)
	case pending > 0:
		reason = fmt.Sprintf("exited with %d unanswered input(s), status %d", pending, code)
	}
	if werr != nil {
		reason += ": " + werr.Error()
	}
	if e := s.proc.Stderr(); e != "" {
		// Redact the whole tail, then keep its end: the last line is the error.
		e = s.a.redactor().Redact(e)
		if len(e) > 700 {
			e = e[len(e)-700:]
		}
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
	if s.a.FixedWrite != nil {
		s.handleFixed(line)
		return
	}
	ctx := context.Background()
	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		_ = s.em.Unknown(ctx, "malformed JSON line", string(line))
		return
	}
	switch w.Type {
	case "system":
		switch w.Subtype {
		case "init", "api_retry", "status", "compact_boundary", "hook_started", "hook_response":
			// hook_* are session-hook progress that real claude emits on start.
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
				_ = s.em.Unknown(ctx, "user block type "+b.Type, string(line))
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
		ok := w.Subtype == "success" && !w.IsError
		s.mu.Lock()
		s.results++
		s.sent = 0 // real claude merges queued inputs into one turn, so a result answers every input sent before it
		s.lastOK = ok
		s.lastSub = w.Subtype
		s.mu.Unlock()
		if !ok {
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

// readLines is proc.ReadLines (bounded line reader).
var readLines = proc.ReadLines
