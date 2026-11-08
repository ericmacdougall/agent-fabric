package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxUnknownRaw bounds the raw text kept for an unclassified harness event.
const MaxUnknownRaw = 2048

// MaxText bounds one message text.
const MaxText = 64 << 10

// SinkTimeout bounds one Sink.Emit call, so a hung sink cannot hold the
// emitter (and every session goroutine behind it) forever.
const SinkTimeout = 30 * time.Second

// Redactor scrubs secret shapes from text. It matches agent.Redactor, so the
// native loop's redactor can be reused.
type Redactor interface{ Redact(s string) string }

var secretShapes = regexp.MustCompile(
	`sk-[A-Za-z0-9_-]{16,}|sk_(?:live|test)_[A-Za-z0-9]{10,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
		`AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|xox[abprs]-[A-Za-z0-9-]{10,}|` +
		`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*|` +
		`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)|` +
		`(?i)(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}|` +
		`(?i)"?(?:password|passwd|secret|token|api[_-]?key|authorization|private[_-]?key)"?\s*[=:]\s*"[^"]*"|` +
		`(?i)(?:password|passwd|secret|token|api[_-]?key)\s*[=:]\s*\S+|` +
		`[a-z][a-z0-9+.-]*://[^\s/:@"']*:[^\s/@"']+@[^\s"']+|` +
		`postgres(?:ql)?://[^\s"']+`)

// zeroWidth characters are removed before redaction so they cannot split a
// secret-shaped token or a key name.
var zeroWidth = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u200e", "", "\u200f", "", "\u2060", "", "\ufeff", "", "\u00ad", "")

type patternRedactor struct{}

func (patternRedactor) Redact(s string) string {
	return secretShapes.ReplaceAllString(zeroWidth.Replace(s), "[REDACTED]")
}

// PatternRedactor is a conservative default redactor for common secret
// shapes (provider keys, JWTs, private keys, bearer and basic credentials,
// key=value and quoted JSON credentials, URLs with credentials). Production
// wiring should compose it with the caller's own redactor.
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

// emitLocked sends one event; the caller holds e.mu. A sink error or panic
// leaves Seq unchanged (no gap), is recorded, and is returned.
func (e *Emitter) emitLocked(ctx context.Context, k Kind, payload any, isFinal bool) (err error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e.seq++
	ev := Event{Version: EventVersion, Kind: k, Harness: e.harness, Session: e.session, Seq: e.seq, Payload: b}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sink panic: %v", r)
		}
		if err != nil {
			e.seq-- // the event was not recorded: keep Seq gap-free
			if e.err == nil {
				e.err = err
			}
			return
		}
		if isFinal {
			e.final = true
		}
	}()
	sctx, cancel := context.WithTimeout(ctx, SinkTimeout)
	defer cancel()
	return e.sink.Emit(sctx, ev)
}

func (e *Emitter) emit(ctx context.Context, k Kind, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.final {
		return ErrAlreadyFinal
	}
	if e.err != nil {
		return e.err
	}
	return e.emitLocked(ctx, k, payload, false)
}

func (e *Emitter) clean(s string, max int) string {
	s = e.red.Redact(zeroWidth.Replace(s))
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
	return e.emit(ctx, KindTurn, Turn{Index: index})
}

// ToolCall emits KindToolCall with a digest of input, never the input.
func (e *Emitter) ToolCall(ctx context.Context, id, name string, input []byte, status string) error {
	sum := sha256.Sum256(input)
	return e.emit(ctx, KindToolCall, ToolCall{
		ID: e.clean(id, 256), Name: e.clean(name, 256),
		InputSHA256: hex.EncodeToString(sum[:]), Status: e.clean(status, 32),
	})
}

// Message emits KindMessage with redacted, bounded text.
func (e *Emitter) Message(ctx context.Context, role, text string) error {
	return e.emit(ctx, KindMessage, Message{Role: e.clean(role, 32), Text: e.clean(text, MaxText)})
}

// Usage emits KindUsage.
func (e *Emitter) Usage(ctx context.Context, u Usage) error {
	return e.emit(ctx, KindUsage, u)
}

// Unknown emits KindUnknown for something the adapter could not classify.
func (e *Emitter) Unknown(ctx context.Context, why, raw string) error {
	return e.emit(ctx, KindUnknown, Unknown{Why: e.clean(why, 256), Raw: e.clean(raw, MaxUnknownRaw)})
}

// Final emits the last event; see FinalOutcome.
func (e *Emitter) Final(ctx context.Context, f Final) error {
	_, err := e.FinalOutcome(ctx, f)
	return err
}

// FinalOutcome emits the last event and returns the Final as it was actually
// recorded. If any earlier event failed to record, or the final itself does,
// a requested success is rewritten to failed: an unrecorded run is not a
// successful one. The check and the rewrite happen under the same lock as the
// send, so a failure on another goroutine cannot slip in between.
func (e *Emitter) FinalOutcome(ctx context.Context, f Final) (Final, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.final {
		return Final{}, ErrAlreadyFinal
	}
	f.Reason = e.clean(f.Reason, 1024)
	if e.err != nil && f.Outcome == OutcomeDone {
		f.Outcome = OutcomeFailed
		f.Reason = e.clean("event sink failed: "+e.err.Error(), 1024)
	}
	err := e.emitLocked(ctx, KindFinal, f, true)
	if err != nil && f.Outcome == OutcomeDone {
		f.Outcome = OutcomeFailed
		f.Reason = e.clean("event sink failed: "+err.Error(), 1024)
	}
	return f, err
}
