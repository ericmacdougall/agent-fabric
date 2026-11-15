// Package credential holds the M1 credential boundary (goodqa#15966 W3): the
// representation policy shared by every durable sink, the bound credential
// set of an attempt, stream redaction, the secret directory, rotation and
// the per-attempt binding record.
//
// Nothing in this package puts a credential byte into an error, a log line,
// argv or an environment other than the pinned claude launch's. Errors carry
// labels, classes and lengths only.
package credential

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

// Form names one representation-policy form (spec v12 AC-16 (2)).
type Form string

const (
	FormRaw        Form = "raw"
	FormJSON       Form = "json"        // Go's encoder with HTML escaping
	FormJSONNoHTML Form = "json-nohtml" // Go's encoder without HTML escaping
	FormB64        Form = "b64"         // standard, unpadded
	FormB64URL     Form = "b64url"      // URL alphabet, unpadded
	FormHexLower   Form = "hex"
	FormHexUpper   Form = "HEX"
	FormURLUpper   Form = "url"
	FormURLLower   Form = "url-lower"
)

// AllForms is the ordered table of forms. Every sink uses this one table.
var AllForms = []Form{FormRaw, FormJSON, FormJSONNoHTML, FormB64, FormB64URL,
	FormHexLower, FormHexUpper, FormURLUpper, FormURLLower}

// Norm names one of the two AC-16 (3) normalisations.
type Norm string

const (
	NormWS  Norm = "ws"  // delete 0x09-0x0D and 0x20
	NormEsc Norm = "esc" // NormWS, then delete the two-byte JSON escapes \n \r \t
)

// AllNorms is the ordered table of normalisations.
var AllNorms = []Norm{NormWS, NormEsc}

// PublicLiteral is the public `sk-ant-` shape, counted as a literal.
const PublicLiteral = "sk-ant-"

// Encode returns form f of v. It never fails for any byte string.
func Encode(f Form, v []byte) []byte {
	switch f {
	case FormRaw:
		return append([]byte(nil), v...)
	case FormJSON:
		b, _ := json.Marshal(string(v))
		return b[1 : len(b)-1]
	case FormJSONNoHTML:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(string(v))
		b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		return append([]byte(nil), b[1:len(b)-1]...)
	case FormB64:
		return []byte(base64.RawStdEncoding.EncodeToString(v))
	case FormB64URL:
		return []byte(base64.RawURLEncoding.EncodeToString(v))
	case FormHexLower:
		return []byte(hex.EncodeToString(v))
	case FormHexUpper:
		return bytes.ToUpper([]byte(hex.EncodeToString(v)))
	case FormURLUpper:
		return percentEncode(v, "0123456789ABCDEF")
	case FormURLLower:
		return percentEncode(v, "0123456789abcdef")
	}
	return nil
}

// percentEncode escapes every byte outside RFC 3986 unreserved characters.
func percentEncode(v []byte, digits string) []byte {
	out := make([]byte, 0, len(v))
	for _, c := range v {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			out = append(out, c)
			continue
		}
		out = append(out, '%', digits[c>>4], digits[c&15])
	}
	return out
}

func isWS(c byte) bool { return c == ' ' || (c >= 0x09 && c <= 0x0D) }

// Normalizer is the streaming form of both normalisations. Batch and
// streaming callers (redaction, the sweep, pattern generation) all use it, so
// a pattern and the stream it is matched against are normalised identically.
// Esc state carries across Feed calls, so a read-buffer boundary inside an
// escape changes nothing.
type Normalizer struct {
	Mode    Norm
	pending bool  // a backslash is held (NormEsc only)
	pendOff int64 // its source offset
	off     int64 // source offset of the next fed byte
}

// Feed normalises src, calling emit(b, srcOffset) for every kept byte.
func (n *Normalizer) Feed(src []byte, emit func(b byte, off int64)) {
	for _, c := range src {
		off := n.off
		n.off++
		if isWS(c) {
			continue
		}
		if n.Mode != NormEsc {
			emit(c, off)
			continue
		}
		if n.pending {
			n.pending = false
			switch c {
			case 'n', 'r', 't':
				continue // the two-byte escape is deleted
			case '\\':
				emit('\\', n.pendOff)
				emit('\\', off)
				continue
			default:
				emit('\\', n.pendOff)
			}
		}
		if c == '\\' {
			n.pending, n.pendOff = true, off
			continue
		}
		emit(c, off)
	}
}

// Flush emits a held backslash at end of input.
func (n *Normalizer) Flush(emit func(b byte, off int64)) {
	if n.pending {
		n.pending = false
		emit('\\', n.pendOff)
	}
}

// Normalize returns the normalised bytes of src and, for each kept byte, its
// offset in src.
func Normalize(mode Norm, src []byte) (norm []byte, idx []int) {
	norm = make([]byte, 0, len(src))
	idx = make([]int, 0, len(src))
	n := Normalizer{Mode: mode}
	emit := func(b byte, off int64) { norm = append(norm, b); idx = append(idx, int(off)) }
	n.Feed(src, emit)
	n.Flush(emit)
	return norm, idx
}

// NormalizeBytes is Normalize without the offset map.
func NormalizeBytes(mode Norm, src []byte) []byte {
	b, _ := Normalize(mode, src)
	return b
}
