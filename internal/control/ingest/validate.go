// Package ingest validates mc.event/1 lines from an external producer and
// (once the control schema lands) stores them in control.ingested_events
// (goodqa#15586 slice B3). The validator is strict and pure: it never trusts
// the producer's `source`, never accepts a `fabric:` id from outside, rejects
// control and ANSI characters, and gives every rejection a short reason that
// never echoes input text.
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits per call (AC-3).
const (
	MaxCallBytes = 1 << 20
	MaxLines     = 2000
	MaxLineBytes = 64 << 10
	MaxSummary   = 280
)

// Schema is the pinned event version: agent-harness d825d66 mc-event-v1.schema.json.
const Schema = "mc.event/1"

// ReservedSource is the producer of `fab control export`; an external source may not use it.
const ReservedSource = "fabric"

var (
	idRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/#-]{0,199}$`)
	tsRE       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$`)
	sourceRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`) // control.ingested_events.id allows a 32-char source prefix
	kinds      = map[string]bool{"wal": true, "push": true, "release": true, "deploy": true, "ticket": true, "chat": true, "owner": true, "initiative": true, "system": true}
	severities = map[string]bool{"info": true, "warn": true, "high": true}
	topFields  = map[string]bool{"schema": true, "id": true, "ts": true, "kind": true, "source": true, "host": true, "actor": true, "subject": true,
		"summary": true, "severity": true, "count": true, "tenant": true, "historic": true}
	actorFields   = map[string]bool{"runtime": true, "session": true, "agent": true}
	subjectFields = map[string]bool{"repo": true, "branch": true, "ref": true, "ticket": true, "initiative": true}
)

// ValidSource reports whether name may pin an external producer.
func ValidSource(name string) error {
	if !sourceRE.MatchString(name) {
		return errors.New("source must match ^[a-z][a-z0-9_-]{0,31}$")
	}
	if name == ReservedSource {
		return errors.New("source \"fabric\" is reserved for fab control export")
	}
	return nil
}

// Event is a validated event, normalised: ts in UTC milliseconds, source pinned.
type Event struct {
	ID       string
	TS       time.Time
	Kind     string
	Source   string
	Host     string
	Actor    map[string]string
	Subject  map[string]string
	Summary  string
	Severity string
	Count    int
	Tenant   string
	Historic bool
}

// Reject is a refused line: its 1-based number and a reason code. Reasons are
// fixed strings or fixed strings with a known field name, never input text.
type Reject struct {
	Line   int
	Reason string
}

type reason string

func (r reason) Error() string { return string(r) }

func bad(format string, a ...any) error { return reason(fmt.Sprintf(format, a...)) }

// fieldName makes an unknown field name safe to print.
func fieldName(k string) string {
	if len(k) <= 40 && regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(k) {
		return k
	}
	return "?"
}

// cleanText rejects invalid UTF-8, control characters (ESC and so ANSI
// sequences, NUL, newlines), Unicode line separators and bidi overrides.
func cleanText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		// U+2028/2029 line separators, U+202A-202E and U+2066-2069 bidi controls, U+FEFF BOM.
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xFEFF {
			return false
		}
	}
	return true
}

// ValidateLine validates one JSON line for the pinned source. It returns the
// normalised event or an error whose text is the reason code.
func ValidateLine(line []byte, source string) (Event, error) {
	if len(line) > MaxLineBytes {
		return Event{}, bad("line-too-long")
	}
	if !utf8.Valid(line) {
		return Event{}, bad("not-utf8")
	}
	if err := noDuplicateKeys(line); err != nil {
		return Event{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil || m == nil {
		return Event{}, bad("not-json-object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Event{}, bad("trailing-data")
	}
	for k := range m {
		if !topFields[k] {
			return Event{}, bad("unknown-field:%s", fieldName(k))
		}
	}
	str := func(k string, required bool, max int) (string, bool, error) {
		raw, ok := m[k]
		if !ok {
			if required {
				return "", false, bad("missing:%s", k)
			}
			return "", false, nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true, bad("bad-type:%s", k)
		}
		if utf8.RuneCountInString(s) > max {
			return "", true, bad("too-long:%s", k)
		}
		if !cleanText(s) {
			return "", true, bad("bad-chars:%s", k)
		}
		return s, true, nil
	}

	schema, _, err := str("schema", true, 20)
	if err != nil {
		return Event{}, err
	}
	if schema != Schema {
		return Event{}, bad("unknown-version")
	}
	ev := Event{Source: source, Count: 1, Severity: "info"}
	id, _, err := str("id", true, 200)
	if err != nil {
		return Event{}, err
	}
	if !idRE.MatchString(id) {
		return Event{}, bad("bad-id")
	}
	// Source-prefixed id, and never the fabric namespace from outside (NEG-2).
	if strings.HasPrefix(strings.ToLower(id), ReservedSource+":") {
		return Event{}, bad("reserved-id-prefix")
	}
	if !strings.HasPrefix(id, source+":") || len(id) == len(source)+1 {
		return Event{}, bad("id-not-source-prefixed")
	}
	ev.ID = id
	ts, _, err := str("ts", true, 40)
	if err != nil {
		return Event{}, err
	}
	if !tsRE.MatchString(ts) {
		return Event{}, bad("bad-ts")
	}
	t, perr := time.Parse(time.RFC3339Nano, ts)
	if perr != nil {
		return Event{}, bad("bad-ts")
	}
	ev.TS = t.UTC().Truncate(time.Millisecond)
	kind, _, err := str("kind", true, 20)
	if err != nil {
		return Event{}, err
	}
	if !kinds[kind] {
		return Event{}, bad("unknown-kind")
	}
	ev.Kind = kind
	if s, ok, err := str("source", false, 40); err != nil {
		return Event{}, err
	} else if ok && s != source {
		return Event{}, bad("source-mismatch")
	}
	if ev.Host, _, err = str("host", false, 64); err != nil {
		return Event{}, err
	}
	if ev.Tenant, _, err = str("tenant", false, 64); err != nil {
		return Event{}, err
	}
	if ev.Summary, _, err = str("summary", false, MaxSummary); err != nil {
		return Event{}, err
	}
	if s, ok, err := str("severity", false, 10); err != nil {
		return Event{}, err
	} else if ok {
		if !severities[s] {
			return Event{}, bad("bad-severity")
		}
		ev.Severity = s
	}
	if raw, ok := m["count"]; ok {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil || strings.ContainsAny(string(n), ".eE\"") || raw[0] == '"' {
			return Event{}, bad("bad-count")
		}
		i, err := n.Int64()
		if err != nil || i < 1 || i > 1_000_000_000 {
			return Event{}, bad("bad-count")
		}
		ev.Count = int(i)
	}
	if raw, ok := m["historic"]; ok {
		if err := json.Unmarshal(raw, &ev.Historic); err != nil {
			return Event{}, bad("bad-type:historic")
		}
	}
	for _, f := range []struct {
		name    string
		allowed map[string]bool
		dst     *map[string]string
	}{{"actor", actorFields, &ev.Actor}, {"subject", subjectFields, &ev.Subject}} {
		raw, ok := m[f.name]
		if !ok {
			continue
		}
		var o map[string]json.RawMessage
		if err := json.Unmarshal(raw, &o); err != nil || o == nil {
			return Event{}, bad("bad-type:%s", f.name)
		}
		out := map[string]string{}
		for k, v := range o {
			if !f.allowed[k] {
				return Event{}, bad("unknown-field:%s.%s", f.name, fieldName(k))
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return Event{}, bad("bad-type:%s.%s", f.name, k)
			}
			if utf8.RuneCountInString(s) > 160 {
				return Event{}, bad("too-long:%s.%s", f.name, k)
			}
			if !cleanText(s) {
				return Event{}, bad("bad-chars:%s.%s", f.name, k)
			}
			out[k] = s
		}
		*f.dst = out
	}
	return ev, nil
}

// noDuplicateKeys rejects an object that repeats a key at any depth, since
// parsers disagree on which value wins.
func noDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if seen[k] {
					return bad("duplicate-key")
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // closing delimiter
		return err
	}
	if err := walk(); err != nil {
		var r reason
		if errors.As(err, &r) {
			return r
		}
		return bad("not-json-object") // syntax errors are reported by the main decode; keep one code
	}
	return nil
}

// ErrCallTooLarge refuses the whole call before anything is written.
var ErrCallTooLarge = errors.New("input exceeds the per-call limit")

// Batch is the result of parsing one call's input.
type Batch struct {
	Lines    int // non-empty lines seen
	Accepted []Event
	Rejected []Reject
	Dups     int // accepted lines repeating an earlier id in this input (dropped)
}

// Parse reads at most MaxCallBytes and MaxLines of JSON lines. A call over
// either limit is refused whole (ErrCallTooLarge) and nothing is returned for
// writing; a bad line is rejected on its own and the rest continue.
func Parse(r io.Reader, source string) (Batch, error) {
	if err := ValidSource(source); err != nil {
		return Batch{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxCallBytes+1))
	if err != nil {
		return Batch{}, err
	}
	if len(data) > MaxCallBytes {
		return Batch{}, fmt.Errorf("%w: more than %d bytes", ErrCallTooLarge, MaxCallBytes)
	}
	var b Batch
	seen := map[string]bool{}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1] // the newline that ends the last line
	}
	if len(lines) > MaxLines {
		return Batch{}, fmt.Errorf("%w: more than %d lines", ErrCallTooLarge, MaxLines)
	}
	for n, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		b.Lines++
		ev, err := ValidateLine(line, source)
		if err != nil {
			var rs reason
			if !errors.As(err, &rs) {
				rs = "invalid"
			}
			b.Rejected = append(b.Rejected, Reject{Line: n + 1, Reason: string(rs)})
			continue
		}
		if seen[ev.ID] {
			b.Dups++
			continue
		}
		seen[ev.ID] = true
		b.Accepted = append(b.Accepted, ev)
	}
	return b, nil
}
