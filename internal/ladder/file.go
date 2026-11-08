package ladder

// Reading escalation.py state and model_tiers.json from disk for
// `fab ladder status` (goodqa#15324). Inputs are bounded, regular,
// non-symlink files decoded strictly: exact-case keys only, one JSON value,
// valid UTF-8, typed fields. Anything escalation.py would treat as corrupt,
// or that Go's lenient decoder would read differently from Python's, is
// refused rather than answered. Errors name the field, never its content.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// MaxInput is escalation.py's MAX_STATE; MaxPolicy bounds the tier table.
const (
	MaxInput  = 1 << 20
	MaxPolicy = 64 << 10
)

// ErrInput marks a refused input file.
var ErrInput = errors.New("ladder input refused")

// safeKey is escalation.py SAFE_KEY: task names are file stems.
var safeKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var rungName = regexp.MustCompile(`^R[0-9]+$`)

// ReadInput reads a regular file of at most limit bytes. The final path
// component is opened with O_NOFOLLOW and O_NONBLOCK and the type is
// checked on the open descriptor, so a swap to a symlink or FIFO after a
// path check cannot redirect or hang the read.
func ReadInput(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrInput, path)
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInput, path)
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInput, path, limit)
	}
	return body, nil
}

// object decodes exactly one JSON object with valid UTF-8 and nothing
// after it. Keys are kept exactly (a map, not struct field folding).
func object(where string, body []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("%w: %s: not valid UTF-8", ErrInput, where)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("%w: %s: not a JSON object", ErrInput, where)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: %s: data after the JSON object", ErrInput, where)
	}
	return m, nil
}

func kind(raw json.RawMessage) byte {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return 0
	}
	switch c := t[0]; {
	case c == '"', c == '{', c == '[', c == 'n', c == 't', c == 'f':
		return c
	default:
		return '0' // number
	}
}

func refuse(where, field, why string) error {
	return fmt.Errorf("%w: %s: %s %s", ErrInput, where, field, why)
}

// str decodes a JSON string and refuses U+FFFD, which is what Go makes of
// lone surrogates such as \ud800; Python keeps them distinct.
func str(where, field string, raw json.RawMessage) (string, error) {
	if kind(raw) != '"' {
		return "", refuse(where, field, "must be a string")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", refuse(where, field, "is not a valid string")
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		return "", refuse(where, field, "contains an invalid or replacement character")
	}
	return s, nil
}

func optStr(where, field string, m map[string]json.RawMessage) (*string, error) {
	raw, ok := m[field]
	if !ok || kind(raw) == 'n' {
		return nil, nil
	}
	s, err := str(where, field, raw)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// integer accepts only an integral JSON number token (Python int).
func integer(where, field string, raw json.RawMessage) (int, error) {
	t := string(bytes.TrimSpace(raw))
	if kind(raw) != '0' || strings.ContainsAny(t, ".eE") {
		return 0, refuse(where, field, "must be an integer")
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, refuse(where, field, "is out of range")
	}
	return n, nil
}

func number(where, field string, raw json.RawMessage) (json.Number, error) {
	if kind(raw) != '0' {
		return "", refuse(where, field, "must be a number")
	}
	n := json.Number(bytes.TrimSpace(raw))
	if _, err := pyNumber(string(n)); err != nil {
		return "", refuse(where, field, "is not a finite number")
	}
	return n, nil
}

func boolean(where, field string, raw json.RawMessage) (bool, error) {
	switch kind(raw) {
	case 't':
		return true, nil
	case 'f':
		return false, nil
	}
	return false, refuse(where, field, "must be a bool")
}

// DecodeState parses an escalation.py state file the way Ledger._read
// accepts it: schema 1, task equal to the file stem, an event list.
func DecodeState(path string, body []byte) (LedgerState, error) {
	where := filepath.Base(path)
	m, err := object(where, body)
	if err != nil {
		return LedgerState{}, err
	}
	var st LedgerState
	schema, ok := m["schema"]
	if !ok {
		return st, refuse(where, "schema", "is missing")
	}
	if n, err := integer(where, "schema", schema); err != nil || n != 1 {
		return st, refuse(where, "schema", "must be 1")
	}
	raw, ok := m["task"]
	if !ok {
		return st, refuse(where, "task", "is missing")
	}
	if st.Task, err = str(where, "task", raw); err != nil {
		return st, err
	}
	if !safeKey.MatchString(st.Task) {
		return st, refuse(where, "task", "is not a safe task key")
	}
	if stem := strings.TrimSuffix(filepath.Base(path), ".json"); stem != st.Task {
		return st, refuse(where, "task", "does not match the file name")
	}
	raw, ok = m["events"]
	if !ok || kind(raw) != '[' {
		return st, refuse(where, "events", "must be a list")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return st, refuse(where, "events", "must be a list")
	}
	st.Events = make([]LedgerEvent, 0, len(items))
	for i, item := range items {
		ew := fmt.Sprintf("%s event %d", where, i)
		if kind(item) != '{' {
			return st, refuse(ew, "event", "must be an object")
		}
		var em map[string]json.RawMessage
		if err := json.Unmarshal(item, &em); err != nil {
			return st, refuse(ew, "event", "must be an object")
		}
		var e LedgerEvent
		cls, err := optStr(ew, "class", em)
		if err != nil {
			return st, err
		}
		if cls != nil {
			e.Class = *cls
		}
		if e.Fingerprint, err = optStr(ew, "fingerprint", em); err != nil {
			return st, err
		}
		if e.Head, err = optStr(ew, "head", em); err != nil {
			return st, err
		}
		st.Events = append(st.Events, e)
	}
	if raw, ok := m["launches"]; ok {
		if st.Launches, err = integer(where, "launches", raw); err != nil {
			return st, err
		}
	}
	st.SpendUSD = "0"
	if raw, ok := m["spend_usd"]; ok {
		if st.SpendUSD, err = number(where, "spend_usd", raw); err != nil {
			return st, err
		}
	}
	return st, nil
}

// DecodePolicy parses the ladder fields of model_tiers.json with the type
// rules of model_tiers.validate for those fields. Model, family and effort
// tables are not read here; the harness validates them.
func DecodePolicy(path string, body []byte) (Policy, error) {
	where := filepath.Base(path)
	m, err := object(where, body)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	raw, ok := m["lowest_rung"]
	if !ok {
		return p, refuse(where, "lowest_rung", "is missing")
	}
	if p.LowestRung, err = str(where, "lowest_rung", raw); err != nil {
		return p, err
	}
	raw, ok = m["failures_per_rung"]
	if !ok {
		return p, refuse(where, "failures_per_rung", "is missing")
	}
	if p.FailuresPerRung, err = integer(where, "failures_per_rung", raw); err != nil {
		return p, err
	}
	raw, ok = m["rungs"]
	if !ok || kind(raw) != '{' {
		return p, refuse(where, "rungs", "must be an object")
	}
	var rungs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rungs); err != nil || len(rungs) == 0 {
		return p, refuse(where, "rungs", "must be a non-empty object")
	}
	p.Rungs = map[string]PolicyRung{}
	for name, body := range rungs {
		if !rungName.MatchString(name) || kind(body) != '{' {
			return p, refuse(where, "rungs", "has a bad rung entry")
		}
		var rm map[string]json.RawMessage
		if err := json.Unmarshal(body, &rm); err != nil {
			return p, refuse(where, "rungs."+name, "must be an object")
		}
		var r PolicyRung
		for _, f := range []struct {
			key string
			dst *bool
		}{{"writable", &r.Writable}, {"needs_user_permission", &r.NeedsUserPermission}} {
			v, ok := rm[f.key]
			if !ok {
				return p, refuse(where, "rungs."+name+"."+f.key, "is missing")
			}
			if *f.dst, err = boolean(where, "rungs."+name+"."+f.key, v); err != nil {
				return p, err
			}
		}
		p.Rungs[name] = r
	}
	raw, ok = m["ladder_limits"]
	if !ok || kind(raw) != '{' {
		return p, refuse(where, "ladder_limits", "must be an object")
	}
	var lm map[string]json.RawMessage
	if err := json.Unmarshal(raw, &lm); err != nil {
		return p, refuse(where, "ladder_limits", "must be an object")
	}
	for _, f := range []struct {
		key string
		dst *int
	}{{"max_review_launches", &p.Limits.MaxReviewLaunches}, {"max_rung_climbs", &p.Limits.MaxRungClimbs}} {
		v, ok := lm[f.key]
		if !ok {
			return p, refuse(where, "ladder_limits."+f.key, "is missing")
		}
		if *f.dst, err = integer(where, "ladder_limits."+f.key, v); err != nil {
			return p, err
		}
	}
	v, ok := lm["max_total_review_usd"]
	if !ok {
		return p, refuse(where, "ladder_limits.max_total_review_usd", "is missing")
	}
	if p.Limits.MaxTotalReviewUSD, err = number(where, "ladder_limits.max_total_review_usd", v); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// StatusFromFiles computes Status from a state file and a policy file.
func StatusFromFiles(statePath, policyPath string) (LadderStatus, error) {
	stateBody, err := ReadInput(statePath, MaxInput)
	if err != nil {
		return LadderStatus{}, err
	}
	policyBody, err := ReadInput(policyPath, MaxPolicy)
	if err != nil {
		return LadderStatus{}, err
	}
	st, err := DecodeState(statePath, stateBody)
	if err != nil {
		return LadderStatus{}, err
	}
	pol, err := DecodePolicy(policyPath, policyBody)
	if err != nil {
		return LadderStatus{}, err
	}
	return Status(pol, st)
}

var printable = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// shortFingerprint prints the first 12 characters of a plain fingerprint
// (escalation.py writes sha256 hex). Anything else is shown as a digest so
// control characters or partial runes never reach the terminal.
func shortFingerprint(fp *string) string {
	if fp == nil {
		return "-"
	}
	if printable.MatchString(*fp) {
		if len(*fp) > 12 {
			return (*fp)[:12]
		}
		return *fp
	}
	sum := sha256.Sum256([]byte(*fp))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// Format renders a status as text, or as indented JSON.
func Format(s LadderStatus, asJSON bool) (string, error) {
	if asJSON {
		b, err := json.MarshalIndent(s, "", " ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	}
	blocked := "-"
	if len(s.BlockedBy) > 0 {
		blocked = strings.Join(s.BlockedBy, ",")
	}
	return fmt.Sprintf("task %s\nrung %s\nfailures_at_rung %d\nclimbs %d\nfingerprint %s\nblocked_by %s\nneeds_user_permission %t\n",
		s.Task, s.Rung, s.FailuresAtRung, s.Climbs, shortFingerprint(s.Fingerprint), blocked, s.NeedsUserPermission), nil
}
