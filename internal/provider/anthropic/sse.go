package anthropic

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SSELimits bound what one response may make the runner buffer (a hostile or
// broken server must not exhaust memory or loop forever).
type SSELimits struct {
	MaxLine   int   // bytes in one line
	MaxEvent  int   // bytes in one event's data
	MaxBytes  int64 // bytes in the whole stream
	MaxEvents int   // events in the whole stream
}

// DefaultSSELimits are generous for real traffic and finite.
func DefaultSSELimits() SSELimits {
	return SSELimits{MaxLine: 1 << 20, MaxEvent: 4 << 20, MaxBytes: 64 << 20, MaxEvents: 500000}
}

// ErrSSELimit is returned when a stream exceeds a bound; the response is
// abandoned and nothing in it is executed.
var ErrSSELimit = errors.New("sse limit exceeded")

// sseEvent is one dispatched server-sent event.
type sseEvent struct {
	Name string
	Data []byte
}

type sseReader struct {
	r      *bufio.Reader
	lim    SSELimits
	bytes  int64
	events int
}

func newSSEReader(r io.Reader, lim SSELimits) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10), lim: lim}
}

// readLine reads one line without ever buffering more than MaxLine bytes.
func (s *sseReader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := s.r.ReadLine()
		if err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		s.bytes += int64(len(chunk)) + 1
		if s.bytes > s.lim.MaxBytes {
			return nil, fmt.Errorf("%w: stream bytes", ErrSSELimit)
		}
		if len(line)+len(chunk) > s.lim.MaxLine {
			return nil, fmt.Errorf("%w: line length", ErrSSELimit)
		}
		line = append(line, chunk...)
		if !isPrefix {
			return line, nil
		}
	}
}

// Next returns the next complete event. A stream that ends in the middle of
// an event returns io.ErrUnexpectedEOF: an event is dispatched only by its
// terminating blank line, so a truncated tail is never delivered.
func (s *sseReader) Next() (sseEvent, error) {
	var name string
	var data bytes.Buffer
	have := false
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && have {
				return sseEvent{}, io.ErrUnexpectedEOF
			}
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if !have {
				continue
			}
			s.events++
			if s.events > s.lim.MaxEvents {
				return sseEvent{}, fmt.Errorf("%w: event count", ErrSSELimit)
			}
			return sseEvent{Name: name, Data: bytes.Clone(data.Bytes())}, nil
		}
		if line[0] == ':' {
			continue
		}
		field, val, _ := strings.Cut(string(line), ":")
		val = strings.TrimPrefix(val, " ")
		have = true
		switch field {
		case "event":
			name = val
		case "data":
			if data.Len()+len(val)+1 > s.lim.MaxEvent {
				return sseEvent{}, fmt.Errorf("%w: event size", ErrSSELimit)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(val)
		}
	}
}
