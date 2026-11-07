package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CanonicalJSON re-serialises a JSON document with object keys sorted, no
// insignificant whitespace, no HTML escaping and numbers kept verbatim, so two
// documents that differ only in key order or spacing produce identical bytes.
// It is the one canonicaliser for effect keys (H1, REPORT.md:304-311):
// executors receive the key, they never compute it.
//
// Size is bounded: a request larger than MaxRequestBytes is refused (R25).
func CanonicalJSON(raw []byte) ([]byte, error) { return canonicalJSON(raw, true) }

// canonicalJSON with strict false keeps every number verbatim, as the
// canonicaliser did before red-team F3. The command hash uses it, so a
// command accepted before F3 (an exponent in its request) still replays
// under its old hash; the effect intake stays strict.
func canonicalJSON(raw []byte, strict bool) ([]byte, error) {
	if len(raw) > MaxRequestBytes {
		return nil, fmt.Errorf("%w: request larger than %d bytes", ErrInvalidTransition, MaxRequestBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: request is not JSON: %v", ErrInvalidTransition, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the JSON request", ErrInvalidTransition)
	}
	if n := nonCanonicalNumber(v); strict && n != "" {
		// Red-team F3: storage (jsonb) re-renders such a number (1e2 as 100,
		// -0 as 0), so a hash of the literal would never match the stored row.
		return nil, fmt.Errorf("%w: number %s is not in canonical form (no exponent, no negative zero)", ErrInvalidTransition, n)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map keys are sorted by encoding/json
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MaxRequestBytes bounds an effect request body.
const MaxRequestBytes = 256 * 1024

// EffectBase is the stable identity of one effect request: the hash of the
// effect kind and the canonical request. Two requests with the same base are
// the same business action; how many times the mission has asked for it is
// the occurrence (H1).
func EffectBase(kind string, request []byte) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("%w: effect kind", ErrInvalidTransition)
	}
	canon, err := CanonicalJSON(request)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{'\n'})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EffectIDFor derives the effect id from its base and occurrence, so the id
// itself says "the second identical ticket comment of this mission".
func EffectIDFor(base string, occurrence int) EffectID {
	return EffectID(fmt.Sprintf("e_%s_%d", base[:24], occurrence))
}

// nonCanonicalNumber returns the first number literal that jsonb would
// render differently: one with an exponent, or a negative zero.
func nonCanonicalNumber(v any) string {
	switch x := v.(type) {
	case json.Number:
		n := string(x)
		if strings.ContainsAny(n, "eE") || (strings.HasPrefix(n, "-") && strings.Trim(n[1:], "0.") == "") {
			return n
		}
	case map[string]any:
		for _, e := range x {
			if n := nonCanonicalNumber(e); n != "" {
				return n
			}
		}
	case []any:
		for _, e := range x {
			if n := nonCanonicalNumber(e); n != "" {
				return n
			}
		}
	}
	return ""
}
