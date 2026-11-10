package codex

import (
	"bytes"
	"encoding/json"
	"io"
)

// Bounds ported from the agent-harness console's Codex binding. They keep a
// misbehaving or hostile protocol peer from growing adapter memory.
const (
	maxPendingFrames  = 128
	maxPendingBytes   = 256 * 1024
	maxPendingDone    = 16
	maxCompletedItems = 1024
	maxItemIDBytes    = 1024
)

// uniqueObject decodes a JSON object and refuses duplicate keys. Last-key-wins
// decoding cannot establish an unambiguous identity, so identity fields are
// only read from objects that pass this check.
func uniqueObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		if _, dup := fields[key]; dup {
			return nil, false
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, false
		}
		fields[key] = v
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// frameTurn returns the turn a notification belongs to, or "" when the
// identity is missing, malformed, duplicated or inconsistent. Content frames
// need turnId; a completion needs turn.id. Every supplied identity must be
// valid and agree; nothing is inferred from the thread.
func frameTurn(raw json.RawMessage, completion bool) string {
	fields, ok := uniqueObject(raw)
	if !ok {
		return ""
	}
	var envelope, nested string
	if v, exists := fields["turnId"]; exists {
		if json.Unmarshal(v, &envelope) != nil || envelope == "" {
			return ""
		}
	} else if !completion {
		return ""
	}
	if v, exists := fields["turn"]; exists {
		turn, valid := uniqueObject(v)
		if !valid {
			return ""
		}
		if json.Unmarshal(turn["id"], &nested) != nil || nested == "" {
			return ""
		}
	} else if completion {
		return ""
	}
	if envelope != "" && nested != "" && envelope != nested {
		return ""
	}
	if completion {
		return nested
	}
	return envelope
}
