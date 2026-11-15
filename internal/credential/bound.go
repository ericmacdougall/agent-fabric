package credential

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Kind names a credential kind bound to an attempt.
type Kind string

const (
	KindToken  Kind = "claude-oauth" // the subscription token version (AC-13)
	KindGoodQA Kind = "goodqa-key"
	KindDSN    Kind = "dsn-password"
)

// Token value ceilings (AC-16 (4)).
const (
	MinTokenLen = 32
	MaxTokenLen = 512
	// GramLen is the AC-15 / AC-10 fragment length.
	GramLen = 8
)

// ErrCredentialLength is the AC-16 (4) refusal. Its message carries only the
// length.
var ErrCredentialLength = errors.New("credential_length")

// Secret is one credential value with its kind and version id. Its fields are
// unexported and every printing path redacts it.
type Secret struct {
	kind    Kind
	version string
	value   []byte
}

// NewSecret copies value. version is a version id, never a value.
func NewSecret(kind Kind, version string, value []byte) Secret {
	return Secret{kind: kind, version: version, value: append([]byte(nil), value...)}
}

func (s Secret) Kind() Kind      { return s.kind }
func (s Secret) Version() string { return s.version }
func (s Secret) Len() int        { return len(s.value) }
func (s Secret) String() string {
	return fmt.Sprintf("credential.Secret{%s:%s len=%d}", s.kind, s.version, len(s.value))
}
func (s Secret) GoString() string             { return s.String() }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Value returns a copy of the value. Only the claude launch environment
// (AC-14) and the sweep input builder (AC-16) call it.
func (s Secret) Value() []byte { return append([]byte(nil), s.value...) }

// CheckTokenLength applies AC-16 (4) to a token value.
func CheckTokenLength(n int) error {
	if n < MinTokenLen || n > MaxTokenLen {
		return fmt.Errorf("%w: token is %d bytes, must be %d to %d", ErrCredentialLength, n, MinTokenLen, MaxTokenLen)
	}
	return nil
}

// pattern is one normalised form of one bound value.
type pattern struct {
	label string // {kind}:{version}:{form}
	kind  Kind
	form  Form
	norm  Norm
	b     []byte
}

// Bound is the set of credential values bound to one attempt (AC-18): the
// token version the attempt was given, and the GoodQA key and DSN passwords
// fabd held. It is what redaction (AC-15), the pre-submit scan (AC-10) and
// the sweep input (AC-16) are built from. It never prints a value.
type Bound struct {
	token  Secret
	others []Secret

	patterns []pattern           // whole-value patterns, all values, deduplicated per norm
	grams    map[uint64]struct{} // every 8-byte window of every normalised token form
	filter   []uint64            // a 2^20-bit prefilter over grams
	alnum    map[uint64]struct{} // AC-10: 8-char windows of alnum-stripped token forms
	// secGrams and secAlnum are the windows of the forms of the token's
	// secret part alone (the value after PublicPrefix). Only a run holding
	// one of them, an 8-byte fragment from the secret part, is a credential
	// exposure; any other token window (the public prefix, or a window that
	// straddles it with fewer than 8 secret bytes) is the public literal
	// (owner ruling 2026-11-14, agent ad1ff08b576984a4d). Coverage is
	// unchanged: every token window is still redacted.
	// The window length is the encoded length of 8 secret bytes in that
	// form (SecretWindow): 8 for raw, JSON and URL forms, 11 for base64, 16
	// for hex. An 8-character hex window is only 4 bytes and matched random
	// hex ids in clean text (goodqa#16040 corpus 2 at b19789e: 5 of 4000).
	secWin   map[int]map[string]struct{} // by length, normalised forms
	secAlnum map[int]map[string]struct{} // by length, alnum-stripped forms
	tokLabel string
}

func (b *Bound) String() string {
	return fmt.Sprintf("credential.Bound{token=%s:%s others=%d}", b.token.kind, b.token.version, len(b.others))
}
func (b *Bound) GoString() string             { return b.String() }
func (b *Bound) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Token returns the bound token version.
func (b *Bound) Token() Secret { return b.token }

// Others returns the bound GoodQA key and DSN passwords.
func (b *Bound) Others() []Secret { return append([]Secret(nil), b.others...) }

// NewBound builds the matcher for an attempt. The token must be 32 to 512
// bytes. Other values may be any non-empty length.
func NewBound(token Secret, others ...Secret) (*Bound, error) {
	if token.kind != KindToken {
		return nil, fmt.Errorf("credential: bound token has kind %q", token.kind)
	}
	if err := CheckTokenLength(len(token.value)); err != nil {
		return nil, err
	}
	b := &Bound{token: token, grams: map[uint64]struct{}{}, alnum: map[uint64]struct{}{},
		secWin: map[int]map[string]struct{}{}, secAlnum: map[int]map[string]struct{}{}, filter: make([]uint64, 1<<14)}
	b.addSecret(token.value, len(PublicPrefix(token.value)))
	b.tokLabel = string(token.kind) + ":" + token.version
	for _, o := range others {
		if o.kind == KindToken {
			return nil, errors.New("credential: a second token version cannot be bound")
		}
		if len(o.value) == 0 {
			return nil, fmt.Errorf("credential: bound %s:%s is empty", o.kind, o.version)
		}
		b.others = append(b.others, o)
	}
	for _, s := range append([]Secret{token}, b.others...) {
		seen := map[string]bool{}
		for _, f := range AllForms {
			enc := Encode(f, s.value)
			for _, n := range AllNorms {
				p := NormalizeBytes(n, enc)
				key := string(n) + "\x00" + string(p)
				if seen[key] || len(p) == 0 {
					continue
				}
				seen[key] = true
				b.patterns = append(b.patterns, pattern{label: string(s.kind) + ":" + s.version + ":" + string(f),
					kind: s.kind, form: f, norm: n, b: p})
				if s.kind == KindToken {
					for i := 0; i+GramLen <= len(p); i++ {
						g := binary.BigEndian.Uint64(p[i:])
						b.grams[g] = struct{}{}
						h := gramHash(g)
						b.filter[h>>6] |= 1 << (h & 63)
					}
				}
			}
			if s.kind == KindToken {
				st := stripAlnum(enc)
				for i := 0; i+GramLen <= len(st); i++ {
					b.alnum[binary.BigEndian.Uint64(st[i:])] = struct{}{}
				}
			}
		}
	}
	return b, nil
}

// publicPrefixRE is the public token shape: `sk-ant-`, a lower-case type
// tag and a dash (sk-ant-oat01- for the AC-17 token).
var publicPrefixRE = regexp.MustCompile(`^sk-ant-[a-z0-9]{1,16}-`)

// PublicPrefix returns the public leading part of a token value, or nil.
func PublicPrefix(v []byte) []byte { return publicPrefixRE.Find(v) }

// addSecret records the windows of every form of the token's secret part,
// v[pre:]. Base64 forms start at the next 3-byte boundary, so their windows
// are exactly the base64 windows of the whole token that hold no prefix bit.
func (b *Bound) addSecret(v []byte, pre int) {
	for _, f := range AllForms {
		src := v[pre:]
		if f == FormB64 || f == FormB64URL {
			src = v[(pre+2)/3*3:]
		}
		enc := Encode(f, src)
		l := SecretWindow(f)
		if b.secWin[l] == nil {
			b.secWin[l], b.secAlnum[l] = map[string]struct{}{}, map[string]struct{}{}
		}
		for _, n := range AllNorms {
			p := NormalizeBytes(n, enc)
			for i := 0; i+l <= len(p); i++ {
				b.secWin[l][string(p[i:i+l])] = struct{}{}
			}
		}
		st := stripAlnum(enc)
		for i := 0; i+l <= len(st); i++ {
			b.secAlnum[l][string(st[i:i+l])] = struct{}{}
		}
	}
}

// SecretWindow is the encoded length of GramLen (8) secret bytes in form f.
func SecretWindow(f Form) int {
	switch f {
	case FormHexLower, FormHexUpper:
		return 2 * GramLen
	case FormB64, FormB64URL:
		return (4*GramLen + 2) / 3
	}
	return GramLen
}

// secretAt reports a window of 8 or more secret bytes, in some form,
// starting at s[i].
func secretAt(sets map[int]map[string]struct{}, s []byte, i int) bool {
	for l, set := range sets {
		if i+l <= len(s) {
			if _, ok := set[string(s[i:i+l])]; ok {
				return true
			}
		}
	}
	return false
}

func gramHash(g uint64) uint64 { return (g * 0x9E3779B97F4A7C15) >> 44 }

func (b *Bound) hasGram(w []byte) bool {
	g := binary.BigEndian.Uint64(w)
	h := gramHash(g)
	if b.filter[h>>6]&(1<<(h&63)) == 0 {
		return false
	}
	_, ok := b.grams[g]
	return ok
}

// PatternCount is the number of distinct whole-value patterns.
func (b *Bound) PatternCount() int { return len(b.patterns) }

func stripAlnum(s []byte) []byte {
	out := make([]byte, 0, len(s))
	for _, c := range s {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return out
}

// Scan classes (AC-10). A class never contains a value.
const (
	ClassFormPrefix = "credential_form:" // followed by a pattern label
	ClassSkAnt      = "sk_ant_literal"
	ClassPostgres   = "postgres_dsn"
	ClassEntropy    = "high_entropy_run"
	ClassFragment   = "credential_fragment"
)

// ScanText is the AC-10 pre-submit scan of model text with no exclusions.
func (b *Bound) ScanText(text string) (hit bool, class string) {
	return b.ScanTextExcluding(text, nil)
}

// ScanTextExcluding is the AC-10 pre-submit scan. exclusions are the exact
// strings an entropy run may equal without blocking: the extract's file
// paths, the mission id and the attempt id. The first matching class is
// returned; the text itself never leaves this function.
func (b *Bound) ScanTextExcluding(text string, exclusions []string) (hit bool, class string) {
	raw := []byte(text)
	for _, n := range AllNorms {
		norm := NormalizeBytes(n, raw)
		for _, p := range b.patterns {
			if p.norm == n && bytes.Contains(norm, p.b) {
				return true, ClassFormPrefix + p.label
			}
		}
	}
	low := strings.ToLower(text)
	if strings.Contains(low, "postgres://") || strings.Contains(low, "postgresql://") {
		return true, ClassPostgres
	}
	if highEntropyRun(text, exclusions) {
		return true, ClassEntropy
	}
	// Raw fragment windows, as redaction counts them: an 8-byte fragment
	// that holds '-' or '_' has fewer than 8 alphanumerics, so the
	// alnum-stripped check below alone would miss it.
	public := false
	for _, n := range AllNorms {
		norm := NormalizeBytes(n, raw)
		for i := 0; i+GramLen <= len(norm); i++ {
			if w := norm[i : i+GramLen]; b.hasGram(w) {
				if secretAt(b.secWin, norm, i) {
					return true, ClassFragment
				}
				public = true
			}
		}
	}
	st := stripAlnum(raw)
	for i := 0; i+GramLen <= len(st); i++ {
		g := binary.BigEndian.Uint64(st[i:])
		if _, ok := b.alnum[g]; ok {
			if secretAt(b.secAlnum, st, i) {
				return true, ClassFragment
			}
			public = true
		}
	}
	// The literal is checked after the fragment windows (AC-35(b)): the
	// prefix plus 8 secret characters is credential_fragment, an exposure,
	// not the literal; the prefix alone or with fewer than 8 secret bytes
	// is the literal and still blocks.
	if public || strings.Contains(text, PublicLiteral) {
		return true, ClassSkAnt
	}
	return false, ""
}

func entropyChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
}

// highEntropyRun reports a run of 32 or more [A-Za-z0-9+/=_-] characters with
// Shannon entropy of at least 4.5 bits per character that equals no
// exclusion.
func highEntropyRun(text string, exclusions []string) bool {
	for i := 0; i < len(text); {
		if !entropyChar(text[i]) {
			i++
			continue
		}
		j := i
		for j < len(text) && entropyChar(text[j]) {
			j++
		}
		run := text[i:j]
		if len(run) >= 32 && !excluded(run, exclusions) && shannon(run) >= 4.5 {
			return true
		}
		i = j
	}
	return false
}

// excluded reports that run equals an exclusion or lies inside one. A
// quoted path splits into runs at '.', so a run inside an excluded path is
// part of that path. ASSUMPTION (owner to confirm with W2's AC-10): the
// spec's "equal a path" covers these sub-runs. Owner ruling 2026-11-14
// (agent ad1ff08b576984a4d): accepted because exclusions are built only from
// the extract file list, mission id and attempt id; it applies only to the
// entropy rule, and containment is one-directional.
func excluded(run string, exclusions []string) bool {
	for _, e := range exclusions {
		if strings.Contains(e, run) {
			return true
		}
	}
	return false
}

func shannon(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	h := 0.0
	n := float64(len(s))
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
