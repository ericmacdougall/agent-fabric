package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxUnknownRaw bounds the raw text kept for an unclassified harness event.
const MaxUnknownRaw = 2048

// MaxText bounds one message text.
const MaxText = 64 << 10

// Redactor scrubs secret shapes from text. It matches agent.Redactor, so the
// native loop's redactor can be reused.
type Redactor interface{ Redact(s string) string }

var secretShapes = regexp.MustCompile(
	`sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|` +
		`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}|` +
		`(?i)(password|passwd|secret|token|api[_-]?key)\s*[=:]\s*\S+|` +
		`postgres(?:ql)?://[^\s"']+`)

type patternRedactor struct{}

func (patternRedactor) Redact(s string) string { return secretShapes.ReplaceAllString(s, "[REDACTED]") }

// PatternRedactor is a conservative default redactor for common secret
// shapes (provider keys, bearer tokens, key=value credentials, database
// URLs). Production wiring should compose it with the caller's own redactor.
func PatternRedactor() Redactor { return patternRedactor{} }

// ErrAlreadyFinal is returned by Emitter methods after Final.
var ErrAlreadyFinal = errors.New("harness: session already emitted its final event")

// Emitter is the one place that stamps version, harness, session and Seq,
// redacts strings, bounds sizes and guarantees a single, last Final. Adapters
// must emit only through it; that is how the contract in the package comment
// holds for every harness and not only for the ones tested.
type Emitter struct {
	harness string
	session string
	sink    Sink
	red     Redactor

	mu    sync.Mutex
	seq   int64
	final bool
	err   error // first sink error; sticky
}

// NewEmitter builds an Emitter. A nil redactor is a programming error.
func NewEmitter(harness, session string, sink Sink, red Redactor) (*Emitter, error) {
	if harness == "" || session == "" || sink == nil || red == nil {
		return nil, errors.New("harness: emitter needs harness, session, sink and redactor")
	}
	return &Emitter{harness: harness, session: session, sink: sink, red: red}, nil
}

// Err returns the first sink error, if any.
func (e *Emitter) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Finished reports whether Final has been emitted.
func (e *Emitter) Finished() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.final
}

func (e *Emitter) emit(ctx context.Context, k Kind, payload any, isFinal bool) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.final {
		return ErrAlreadyFinal
	}
	if e.err != nil && !isFinal {
		return e.err
	}
	e.seq++
	ev := Event{Version: EventVersion, Kind: k, Harness: e.harness, Session: e.session, Seq: e.seq, Payload: b}
	if err := e.sink.Emit(ctx, ev); err != nil {
		e.seq-- // the event was not recorded: keep Seq gap-free
		if e.err == nil {
			e.err = err
		}
		return err
	}
	if isFinal {
		e.final = true
	}
	return nil
}

func (e *Emitter) clean(s string, max int) string {
	s = e.red.Redact(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// Turn emits KindTurn.
func (e *Emitter) Turn(ctx context.Context, index int) error {
	return e.emit(ctx, KindTurn, Turn{Index: index}, false)
}

// ToolCall emits KindToolCall with a digest of input, never the input.
func (e *Emitter) ToolCall(ctx context.Context, id, name string, input []byte, status string) error {
	sum := sha256.Sum256(input)
	return e.emit(ctx, KindToolCall, ToolCall{
		ID: e.clean(id, 256), Name: e.clean(name, 256),
		InputSHA256: hex.EncodeToString(sum[:]), Status: e.clean(status, 32),
	}, false)
}

// Message emits KindMessage with redacted, bounded text.
func (e *Emitter) Message(ctx context.Context, role, text string) error {
	return e.emit(ctx, KindMessage, Message{Role: e.clean(role, 32), Text: e.clean(text, MaxText)}, false)
}

// Usage emits KindUsage.
func (e *Emitter) Usage(ctx context.Context, u Usage) error {
	return e.emit(ctx, KindUsage, u, false)
}

// Unknown emits KindUnknown for something the adapter could not classify.
func (e *Emitter) Unknown(ctx context.Context, why, raw string) error {
	return e.emit(ctx, KindUnknown, Unknown{Why: e.clean(why, 256), Raw: e.clean(raw, MaxUnknownRaw)}, false)
}

// Final emits the last event. If an earlier sink error occurred and the
// caller asked for success, the outcome is rewritten to failed: an
// unrecorded run is not a successful one.
func (e *Emitter) Final(ctx context.Context, f Final) error {
	if err := e.Err(); err != nil && f.Outcome == OutcomeDone {
		f.Outcome = OutcomeFailed
		f.Reason = "event sink failed: " + err.Error()
	}
	f.Reason = e.clean(f.Reason, 1024)
	return e.emit(ctx, KindFinal, f, true)
}
