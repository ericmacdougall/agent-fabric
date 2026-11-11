package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"fabric/internal/harness"
)

// LifecycleSink projects fixed metadata only. No event payload string, prompt,
// tool argument, error reason or provider text is copied to this public stream.
// The caller owns the writer and must use a new 0600 O_EXCL file.
type LifecycleSink struct {
	mu             sync.Mutex
	out            io.Writer
	next           harness.Sink
	session        string
	started, final bool
	err            error
}

var demoID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewLifecycleSink(out io.Writer, next harness.Sink, session string) (*LifecycleSink, error) {
	if out == nil || next == nil || !demoID.MatchString(session) {
		return nil, errors.New("codex: invalid lifecycle sink")
	}
	return &LifecycleSink{out: out, next: next, session: session}, nil
}
func (s *LifecycleSink) Emit(ctx context.Context, e harness.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if e.Harness != Name || e.Session != s.session || s.final {
		return errors.New("codex: lifecycle identity or order mismatch")
	}
	if err := s.next.Emit(ctx, e); err != nil {
		s.err = err
		return err
	}
	if !s.started {
		if err := s.write(1, "started", "info"); err != nil {
			s.err = err
			return err
		}
		s.started = true
	}
	if e.Kind != harness.KindFinal {
		return nil
	}
	var f harness.Final
	if json.Unmarshal(e.Payload, &f) != nil {
		return errors.New("codex: malformed lifecycle final")
	}
	state, severity := "failed", "high"
	switch f.Outcome {
	case harness.OutcomeDone:
		state, severity = "completed", "info"
	case harness.OutcomeStopped:
		state, severity = "stopped", "warn"
	case harness.OutcomeFailed:
	default:
		return errors.New("codex: unsupported lifecycle outcome")
	}
	s.final = true
	s.err = s.write(2, state, severity)
	return s.err
}
func (s *LifecycleSink) write(seq int, state, severity string) error {
	row := struct {
		Schema   string            `json:"schema"`
		ID       string            `json:"id"`
		TS       string            `json:"ts"`
		Kind     string            `json:"kind"`
		Source   string            `json:"source"`
		Actor    map[string]string `json:"actor"`
		Subject  map[string]string `json:"subject"`
		Summary  string            `json:"summary"`
		Severity string            `json:"severity"`
	}{"mc.event/1", fmt.Sprintf("codex_demo:%s:%d", s.session, seq), time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "owner", "codex_demo", map[string]string{"runtime": "codex", "session": s.session}, map[string]string{}, "Codex external provider session " + state, severity}
	b, _ := json.Marshal(row)
	b = append(b, '\n')
	n, err := s.out.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}
