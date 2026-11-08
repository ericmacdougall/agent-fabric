// Package envelope holds the event envelope v2 pieces that fix irreversible
// choices (docs/adr/0001-kernel-scope.md, F1). This file is the canonical
// byte form: JSON Canonicalization Scheme, RFC 8785. The same value always
// produces the same bytes, independent of Go field order, map order, or the
// whitespace and key order a database round trip gives back, so a hash taken
// over them is stable across languages.
//
// Canonicalize is strict on purpose. It rejects what RFC 8785 leaves to the
// caller: duplicate keys, invalid UTF-8, lone surrogates, numbers that are
// not finite, integer literals a float64 cannot hold exactly, trailing data
// and excessive nesting.
package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Limits bound the work done on untrusted input.
const (
	MaxDepth = 64
	MaxBytes = 4 << 20
)

// Errors returned by Canonicalize.
var (
	ErrSyntax        = errors.New("envelope: invalid JSON")
	ErrDuplicateKey  = errors.New("envelope: duplicate object key")
	ErrUTF8          = errors.New("envelope: invalid UTF-8 or lone surrogate")
	ErrNumber        = errors.New("envelope: number is not finite or loses precision")
	ErrTooDeep       = errors.New("envelope: nesting too deep")
	ErrTooLarge      = errors.New("envelope: input too large")
	ErrTrailingBytes = errors.New("envelope: trailing data after the JSON value")
)

// Marshal encodes v with encoding/json and returns its canonical form.
//
// It inherits encoding/json's coercion of invalid UTF-8 to U+FFFD, so two Go
// strings that differ only in invalid bytes marshal to the same bytes. The
// strict rejection of invalid UTF-8 applies to Canonicalize on raw JSON; code
// that must refuse such strings validates them before calling Marshal. Integer
// values beyond +-2^53 are refused (see Canonicalize); carry them as strings.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// Canonicalize returns the RFC 8785 form of one JSON value.
func Canonicalize(in []byte) ([]byte, error) {
	if len(in) > MaxBytes {
		return nil, ErrTooLarge
	}
	p := &parser{b: in}
	p.skip()
	var out bytes.Buffer
	if err := p.value(&out, 0); err != nil {
		return nil, err
	}
	p.skip()
	if p.i != len(p.b) {
		return nil, ErrTrailingBytes
	}
	// Number output can be longer than its input (1e20 prints as 21 digits).
	// Capping the output too keeps "canonical bytes re-canonicalise to
	// themselves" true for every accepted document.
	if out.Len() > MaxBytes {
		return nil, ErrTooLarge
	}
	return out.Bytes(), nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) skip() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(out *bytes.Buffer, depth int) error {
	if p.i >= len(p.b) {
		return ErrSyntax
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object(out, depth)
	case c == '[':
		return p.array(out, depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return err
		}
		writeString(out, s)
		return nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(out)
	default:
		for _, lit := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.b[p.i:], []byte(lit)) {
				p.i += len(lit)
				out.WriteString(lit)
				return nil
			}
		}
		return ErrSyntax
	}
}

func (p *parser) array(out *bytes.Buffer, depth int) error {
	if depth >= MaxDepth { // at most MaxDepth nested containers, arrays and objects alike
		return ErrTooDeep
	}
	p.i++ // [
	out.WriteByte('[')
	p.skip()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		out.WriteByte(']')
		return nil
	}
	for first := true; ; first = false {
		if !first {
			out.WriteByte(',')
		}
		p.skip()
		if err := p.value(out, depth+1); err != nil {
			return err
		}
		p.skip()
		if p.i >= len(p.b) {
			return ErrSyntax
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			out.WriteByte(']')
			return nil
		default:
			return ErrSyntax
		}
	}
}

type member struct {
	key []uint16 // UTF-16 code units: the RFC 8785 sort key
	k   string
	v   []byte
}

func (p *parser) object(out *bytes.Buffer, depth int) error {
	if depth >= MaxDepth {
		return ErrTooDeep
	}
	p.i++ // {
	p.skip()
	var ms []member
	seen := map[string]bool{}
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		out.WriteString("{}")
		return nil
	}
	for {
		p.skip()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return ErrSyntax
		}
		k, err := p.str()
		if err != nil {
			return err
		}
		if seen[k] {
			return ErrDuplicateKey
		}
		seen[k] = true
		p.skip()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return ErrSyntax
		}
		p.i++
		p.skip()
		var vb bytes.Buffer
		if err := p.value(&vb, depth+1); err != nil {
			return err
		}
		ms = append(ms, member{key: utf16.Encode([]rune(k)), k: k, v: vb.Bytes()})
		p.skip()
		if p.i >= len(p.b) {
			return ErrSyntax
		}
		if p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.b[p.i] == '}' {
			p.i++
			break
		}
		return ErrSyntax
	}
	sort.Slice(ms, func(a, b int) bool { return less16(ms[a].key, ms[b].key) })
	out.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			out.WriteByte(',')
		}
		writeString(out, m.k)
		out.WriteByte(':')
		out.Write(m.v)
	}
	out.WriteByte('}')
	return nil
}

func less16(a, b []uint16) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// str parses a JSON string at p.i (which is '"') and returns its value.
func (p *parser) str() (string, error) {
	p.i++
	var sb strings.Builder
	for {
		if p.i >= len(p.b) {
			return "", ErrSyntax
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return sb.String(), nil
		case c < 0x20:
			return "", ErrSyntax
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return "", ErrSyntax
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, err := p.hex4()
				if err != nil {
					return "", err
				}
				switch {
				case utf16.IsSurrogate(r) && r >= 0xDC00:
					return "", ErrUTF8 // low surrogate with no high one
				case utf16.IsSurrogate(r):
					if p.i+1 >= len(p.b) || p.b[p.i] != '\\' || p.b[p.i+1] != 'u' {
						return "", ErrUTF8
					}
					p.i += 2
					lo, err := p.hex4()
					if err != nil {
						return "", err
					}
					if lo < 0xDC00 || lo > 0xDFFF {
						return "", ErrUTF8
					}
					sb.WriteRune(utf16.DecodeRune(r, lo))
				default:
					sb.WriteRune(r)
				}
			default:
				return "", ErrSyntax
			}
		default:
			r, size := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && size <= 1 {
				return "", ErrUTF8
			}
			sb.Write(p.b[p.i : p.i+size])
			p.i += size
		}
	}
}

func (p *parser) hex4() (rune, error) {
	if p.i+4 > len(p.b) {
		return 0, ErrSyntax
	}
	n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, ErrSyntax
	}
	p.i += 4
	return rune(n), nil
}

// writeString emits RFC 8785 string form: only the two-character escapes for
// quote, backslash, \b \f \n \r \t, \u00xx (lower-case hex) for the other
// control characters, and everything else as literal UTF-8.
func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

func (p *parser) number(out *bytes.Buffer) error {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i >= len(p.b) {
		return ErrSyntax
	}
	if p.b[p.i] == '0' {
		p.i++
		if p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			return ErrSyntax // leading zero
		}
	} else if digits() == 0 {
		return ErrSyntax
	}
	isInt := true
	if p.i < len(p.b) && p.b[p.i] == '.' {
		isInt = false
		p.i++
		if digits() == 0 {
			return ErrSyntax
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		isInt = false
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return ErrSyntax
		}
	}
	lit := string(p.b[start:p.i])
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return ErrNumber
	}
	if isInt && lit != "-0" && lit != FormatNumber(f) {
		// An integer literal that a float64 cannot hold exactly would be
		// silently rounded (9007199254740993 becomes ...992), so equal
		// stored values could hash differently across languages. A literal
		// that already is its own RFC 8785 form is kept, which also keeps
		// Canonicalize idempotent on large floats it printed as integers.
		return ErrNumber
	}
	out.WriteString(FormatNumber(f))
	return nil
}

// FormatNumber is the ECMAScript Number::toString form RFC 8785 requires.
func FormatNumber(f float64) string {
	if f == 0 {
		return "0" // also for -0
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// Shortest round-trip digits and a decimal exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	mant, expS, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expS)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digits
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		x := n - 1
		if x < 0 {
			sign = "-"
			x = -x
		}
		if k == 1 {
			s = digits + "e" + sign + strconv.Itoa(x)
		} else {
			s = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(x)
		}
	}
	if neg {
		return "-" + s
	}
	return s
}
