package credential

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"regexp"
)

// Sweep protocol constants (AC-16).
const (
	SweepWindow      = 32
	SweepMaxPatterns = 144
	SweepMaxLen      = 6 * MaxTokenLen // JSON \u00XX escapes
	SweepMaxInput    = 64 << 10
	SweepFPBits      = 20
	MersenneP        = (1 << 61) - 1
	sweepBaseMin     = 1 << 32
	sweepBaseMax     = MersenneP - 1 // inclusive upper bound 2^61-2
)

// SweepEntry is one pattern: its length, window fingerprint, HMAC, the
// normalisation and a label. It holds no byte of the pattern.
type SweepEntry struct {
	Len   int    `json:"len"`
	FP    uint32 `json:"fp"`
	MAC   string `json:"mac"` // base64 HMAC-SHA256(K, p)
	Norm  Norm   `json:"norm"`
	Label string `json:"label"`
}

// SweepInput is the sweep's stdin document: a fresh HMAC key K, a fresh
// rolling base B, the window, the public literal and the entries.
type SweepInput struct {
	Key     string       `json:"key"` // base64, 32 bytes
	Base    uint64       `json:"base"`
	Window  int          `json:"window"`
	Literal string       `json:"literal"`
	Entries []SweepEntry `json:"entries"`
}

// MulMod returns a*b mod 2^61-1 for a, b < 2^61.
func MulMod(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	// x = hi*2^64 + lo, and 2^64 = 8 * 2^61 = 8 (mod p).
	r := (lo & MersenneP) + (lo >> 61) + (hi << 3)
	r = (r & MersenneP) + (r >> 61)
	if r >= MersenneP {
		r -= MersenneP
	}
	return r
}

// Fingerprint is fp(x): the low 20 bits of sum x[i]*B^(W-1-i) mod 2^61-1
// over x[0:W].
func Fingerprint(x []byte, base uint64, w int) uint32 {
	var h uint64
	for i := 0; i < w; i++ {
		h = MulMod(h, base) + uint64(x[i])
		if h >= MersenneP {
			h -= MersenneP
		}
	}
	return uint32(h & (1<<SweepFPBits - 1))
}

func randBase() (uint64, error) {
	var b [8]byte
	span := uint64(sweepBaseMax - sweepBaseMin + 1)
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint64(b[:]) >> 3 // < 2^61
		if v < span*(uint64(1<<61)/span) {
			return sweepBaseMin + v%span, nil
		}
	}
}

// BuildSweepInput computes the sweep input for an attempt from its bound
// token version only (AC-16: the GoodQA key and DSN passwords never reach
// the worker uid). K and B are fresh for every call; call it after the
// worker has stopped.
func BuildSweepInput(b *Bound) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	base, err := randBase()
	if err != nil {
		return nil, err
	}
	in := SweepInput{Key: base64.StdEncoding.EncodeToString(key), Base: base, Window: SweepWindow, Literal: PublicLiteral}
	m := hmac.New(sha256.New, key)
	// Identical patterns are kept once. When N_ws(f) equals N_esc(f) the
	// pattern holds no escape, and every occurrence N_ws finds N_esc also
	// finds, so the single entry carries NormEsc.
	at := map[string]int{}
	for _, p := range b.patterns {
		if p.kind != KindToken {
			continue
		}
		if i, ok := at[string(p.b)]; ok {
			in.Entries[i].Norm = NormEsc
			continue
		}
		at[string(p.b)] = len(in.Entries)
		if len(p.b) < SweepWindow || len(p.b) > SweepMaxLen {
			return nil, fmt.Errorf("credential: sweep pattern %s has length %d outside %d..%d", p.label, len(p.b), SweepWindow, SweepMaxLen)
		}
		m.Reset()
		m.Write(p.b)
		in.Entries = append(in.Entries, SweepEntry{Len: len(p.b), FP: Fingerprint(p.b, base, SweepWindow),
			MAC: base64.StdEncoding.EncodeToString(m.Sum(nil)), Norm: p.norm, Label: p.label})
	}
	if len(in.Entries) > SweepMaxPatterns {
		return nil, errors.New("credential: too many sweep patterns")
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(data) > SweepMaxInput {
		return nil, errors.New("credential: sweep input over 64 KiB")
	}
	return data, nil
}

var labelRE = regexp.MustCompile(`^[a-z0-9-]{1,32}:[A-Za-z0-9._-]{1,64}:[A-Za-z0-9-]{1,16}$`)

// ParseSweepInput strictly validates a sweep input. Short is set when an
// entry has L < W (the sweep is then incomplete, never clean).
func ParseSweepInput(data []byte) (in SweepInput, key []byte, macs [][]byte, short bool, err error) {
	if len(data) > SweepMaxInput {
		return in, nil, nil, false, errors.New("input over 64 KiB")
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, nil, nil, false, errors.New("input is not valid JSON")
	}
	key, err = base64.StdEncoding.DecodeString(in.Key)
	if err != nil || len(key) != 32 {
		return in, nil, nil, false, errors.New("bad key")
	}
	if in.Window != SweepWindow || in.Base < sweepBaseMin || in.Base > sweepBaseMax || in.Literal != PublicLiteral {
		return in, nil, nil, false, errors.New("bad window, base or literal")
	}
	if len(in.Entries) == 0 || len(in.Entries) > SweepMaxPatterns {
		return in, nil, nil, false, errors.New("bad entry count")
	}
	for _, e := range in.Entries {
		mac, err := base64.StdEncoding.DecodeString(e.MAC)
		if err != nil || len(mac) != sha256.Size || (e.Norm != NormWS && e.Norm != NormEsc) ||
			e.Len > SweepMaxLen || e.Len <= 0 || e.FP >= 1<<SweepFPBits || !labelRE.MatchString(e.Label) {
			return in, nil, nil, false, errors.New("bad entry")
		}
		if e.Len < in.Window {
			short = true
		}
		macs = append(macs, mac)
	}
	return in, key, macs, short, nil
}
