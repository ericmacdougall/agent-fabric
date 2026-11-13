package reviewgate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxJSONDepth = 64

// CanonicalJSON produces deterministic UTF-8 JSON: sorted object keys, no
// insignificant whitespace, preserved JSON number lexemes and no HTML escape
// rewriting. Duplicate keys, invalid UTF-8, trailing data and excessive depth
// are malformed rather than silently collapsed.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxProposalBytes {
		return nil, invalid("JSON size %d exceeds the canonical input bound", len(raw))
	}
	if !utf8.Valid(raw) {
		return nil, invalid("JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeJSONValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed JSON: %v", ErrInvalid, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, invalid("trailing JSON value")
		}
		return nil, invalid("trailing data after JSON")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: canonical JSON: %v", ErrInvalid, err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func decodeJSONValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, fmt.Errorf("depth exceeds %d", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		m := make(map[string]any)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, exists := m[key]; exists {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			v, err := decodeJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated object")
		}
		return m, nil
	case '[':
		var a []any
		for dec.More() {
			v, err := decodeJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated array")
		}
		if a == nil {
			a = []any{}
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// CanonicalProposal validates and normalizes the hash subject. Provenance is
// a set, so handles and parent edges are sorted before encoding.
func CanonicalProposal(p Proposal) ([]byte, error) {
	for name, value := range map[string]string{
		"mission": p.MissionID, "session": p.SessionID, "tool": p.Tool,
		"state fingerprint": p.StateFingerprint,
	} {
		if !validText(value, MaxNameBytes) {
			return nil, invalid("%s is empty, too long or not UTF-8", name)
		}
	}
	trimmed := bytes.TrimSpace(p.Args)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalid("tool args must be a JSON object")
	}
	args, err := CanonicalJSON(trimmed)
	if err != nil {
		return nil, err
	}
	if len(p.Provenance) == 0 {
		return nil, invalid("at least one structural provenance handle is required")
	}
	handles, err := normalizeProvenance(p.Provenance)
	if err != nil {
		return nil, err
	}
	// Use a private wire type to ensure future operational fields cannot
	// silently enter the request hash without an explicit version change.
	type wireProposal struct {
		MissionID        string             `json:"mission_id"`
		SessionID        string             `json:"session_id"`
		Tool             string             `json:"tool"`
		Args             json.RawMessage    `json:"args"`
		StateFingerprint string             `json:"state_fingerprint"`
		Provenance       []ProvenanceHandle `json:"provenance"`
		Binding          *Binding           `json:"binding,omitempty"`
	}
	if err := validBinding(p.Binding); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wireProposal{p.MissionID, p.SessionID, p.Tool, args, p.StateFingerprint, handles, p.Binding}); err != nil {
		return nil, fmt.Errorf("%w: canonical proposal: %v", ErrInvalid, err)
	}
	c := bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
	if len(c) > MaxProposalBytes {
		return nil, invalid("canonical proposal size %d exceeds %d", len(c), MaxProposalBytes)
	}
	return c, nil
}

func normalizeProvenance(in []ProvenanceHandle) ([]ProvenanceHandle, error) {
	if len(in) > 128 {
		return nil, invalid("too many provenance handles")
	}
	byID := make(map[string]ProvenanceHandle, len(in))
	for _, h := range in {
		if !validText(h.ID, 128) || strings.ContainsAny(h.ID, " \t\r\n") {
			return nil, invalid("invalid provenance handle")
		}
		switch h.Origin {
		case OriginUser, OriginOperator, OriginTool, OriginExternal, OriginAgent:
		default:
			return nil, invalid("unknown provenance origin %q", h.Origin)
		}
		if _, exists := byID[h.ID]; exists {
			return nil, invalid("duplicate provenance handle %q", h.ID)
		}
		if len(h.DerivedFrom) > 128 {
			return nil, invalid("too many provenance parents")
		}
		h.DerivedFrom = append([]string(nil), h.DerivedFrom...)
		sort.Strings(h.DerivedFrom)
		for i, parent := range h.DerivedFrom {
			if parent == h.ID || (i > 0 && parent == h.DerivedFrom[i-1]) {
				return nil, invalid("self or duplicate provenance edge at %q", h.ID)
			}
		}
		byID[h.ID] = h
	}
	for _, h := range byID {
		for _, parent := range h.DerivedFrom {
			if _, ok := byID[parent]; !ok {
				return nil, invalid("dangling provenance parent %q", parent)
			}
		}
	}
	// DFS rejects cycles. Taint flows from every parent to every derived handle.
	const visiting, done = 1, 2
	marks := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if marks[id] == visiting {
			return invalid("provenance cycle at %q", id)
		}
		if marks[id] == done {
			return nil
		}
		marks[id] = visiting
		for _, parent := range byID[id].DerivedFrom {
			if err := visit(parent); err != nil {
				return err
			}
		}
		marks[id] = done
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	out := make([]ProvenanceHandle, 0, len(byID))
	for _, h := range byID {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func proposalTainted(handles []ProvenanceHandle) bool {
	byID := make(map[string]ProvenanceHandle, len(handles))
	for _, h := range handles {
		byID[h.ID] = h
	}
	memo := map[string]bool{}
	seen := map[string]bool{}
	var tainted func(string) bool
	tainted = func(id string) bool {
		if seen[id] {
			return memo[id]
		}
		seen[id] = true
		h := byID[id]
		memo[id] = h.Tainted || h.Origin == OriginTool || h.Origin == OriginExternal || h.Origin == OriginAgent
		for _, parent := range h.DerivedFrom {
			memo[id] = tainted(parent) || memo[id]
		}
		return memo[id]
	}
	for id := range byID {
		if tainted(id) {
			return true
		}
	}
	return false
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
