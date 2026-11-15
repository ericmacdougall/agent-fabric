package credential

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"os"
	"strings"
	"testing"
)

// synthRand is the source of synthetic credential bytes: crypto/rand,
// or a ChaCha8 stream when FAB_PROP_SEED pre-registers a seed (goodqa#16040
// property runs), so a counted run is reproducible from its recorded seed.
var synthRand io.Reader = rand.Reader

func init() {
	if v := os.Getenv("FAB_PROP_SEED"); v != "" {
		seed, err := hex.DecodeString(v)
		if err != nil || len(seed) != 32 {
			panic("FAB_PROP_SEED must be 64 hex characters")
		}
		synthRand = mrand.NewChaCha8([32]byte(seed))
	}
}

// synthToken generates a synthetic token at test time (never a literal):
// the sk-ant- shape plus n random base64url characters.
func synthToken(t testing.TB, n int) []byte {
	t.Helper()
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := []byte("sk-" + "ant-oat01-")
	for len(out) < n {
		v, err := rand.Int(synthRand, big.NewInt(int64(len(alpha))))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, alpha[v.Int64()])
	}
	return out
}

func synthSecret(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(synthRand, b); err != nil {
		t.Fatal(err)
	}
	const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return b
}

func testBound(t testing.TB, tok []byte) *Bound {
	t.Helper()
	b, err := NewBound(NewSecret(KindToken, "v3", tok),
		NewSecret(KindGoodQA, "aaaaaaaaaaaaaaaa", synthSecret(t, 40)),
		NewSecret(KindDSN, "bbbbbbbbbbbbbbbb", synthSecret(t, 24)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// noGram fails when any 8-byte window of any normalised form of tok
// survives in out (the AC-15 acceptance property).
func noGram(t *testing.T, name string, tok, out []byte) {
	t.Helper()
	grams := map[uint64]bool{}
	for _, f := range AllForms {
		for _, n := range AllNorms {
			p := NormalizeBytes(n, Encode(f, tok))
			for i := 0; i+8 <= len(p); i++ {
				grams[binary.BigEndian.Uint64(p[i:])] = true
			}
		}
	}
	for _, n := range AllNorms {
		norm := NormalizeBytes(n, out)
		for i := 0; i+8 <= len(norm); i++ {
			if grams[binary.BigEndian.Uint64(norm[i:])] {
				t.Fatalf("%s: a fragment of 8 or more bytes survives at normalised offset %d (%s)", name, i, n)
			}
		}
	}
}

func jsonLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "text": text, "n": 42})
	return append(b, '\n')
}

func TestRedactEveryFormBothStreams(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	for _, s := range append([]Secret{b.token}, b.others...) {
		for _, f := range AllForms {
			enc := string(Encode(f, s.value))
			// stdout carries the form inside a JSON string; stderr raw.
			out := jsonLine("before " + enc + " after")
			errb := []byte("err: " + enc + " tail\n")
			r := b.Redact(out, errb)
			if r.Blocked != "" {
				t.Fatalf("%s/%s: blocked %s", s.kind, f, r.Blocked)
			}
			if bytes.Contains(r.Stdout, s.value) || bytes.Contains(r.Stderr, s.value) ||
				bytes.Contains(r.Stdout, []byte(enc)) || bytes.Contains(r.Stderr, []byte(enc)) {
				t.Fatalf("%s/%s: value survives", s.kind, f)
			}
			if len(r.Stdout) != len(out) || len(r.Stderr) != len(errb) {
				t.Fatalf("%s/%s: length changed", s.kind, f)
			}
			if !sameStructure(bytes.TrimSpace(out), bytes.TrimSpace(r.Stdout)) {
				t.Fatalf("%s/%s: structure changed", s.kind, f)
			}
			if !r.Exposed() {
				t.Fatalf("%s/%s: not counted", s.kind, f)
			}
			if !bytes.HasSuffix(r.Stderr, []byte(" tail\n")) || !bytes.HasPrefix(r.Stderr, []byte("err: ")) {
				t.Fatalf("%s/%s: surrounding bytes changed: %q", s.kind, f, r.Stderr)
			}
			if s.kind == KindToken {
				noGram(t, string(f), tok, r.Stdout)
				noGram(t, string(f), tok, r.Stderr)
			}
		}
	}
}

func TestRedactSplitByNewlineEveryOffset(t *testing.T) {
	tok := synthToken(t, 64)
	b := testBound(t, tok)
	for i := 1; i < len(tok); i++ {
		in := []byte("x " + string(tok[:i]) + "\n " + string(tok[i:]) + " y\n")
		r := b.Redact(nil, in)
		want := len(tok) // every token byte rewritten
		got := bytes.Count(r.Stderr, []byte("X"))
		if got < want || !bytes.HasPrefix(r.Stderr, []byte("x ")) || !bytes.HasSuffix(r.Stderr, []byte(" y\n")) {
			t.Fatalf("offset %d: got %q", i, r.Stderr)
		}
		noGram(t, fmt.Sprint("nl", i), tok, r.Stderr)
	}
}

func TestRedactSplitByJSONEscape(t *testing.T) {
	tok := synthToken(t, 64)
	b := testBound(t, tok)
	for _, esc := range []string{`\n`, `\r`, `\t`} {
		for i := 1; i < len(tok); i += 7 {
			line := []byte(`{"type":"assistant","text":"pre ` + string(tok[:i]) + esc + string(tok[i:]) + ` post","n":7}` + "\n")
			r := b.Redact(line, nil)
			if r.Blocked != "" {
				t.Fatalf("blocked %s", r.Blocked)
			}
			if !json.Valid(bytes.TrimSpace(r.Stdout)) {
				t.Fatalf("invalid json: %q", r.Stdout)
			}
			if bytes.Contains(r.Stdout, []byte(esc)) {
				t.Fatalf("escape inside the span not replaced: %q", r.Stdout)
			}
			if !bytes.Contains(r.Stdout, []byte(`"pre `)) || !bytes.Contains(r.Stdout, []byte(` post","n":7}`)) {
				t.Fatalf("surroundings changed: %q", r.Stdout)
			}
			noGram(t, "esc", tok, r.Stdout)
		}
	}
}

// AC-15 / NEG-12: a value split between stdout and stderr, in both
// directions, and into 3, 5 and 13 alternating fragments.
func TestRedactCrossStreamFragments(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	for _, pieces := range []int{2, 3, 5, 13} {
		for _, startOut := range []bool{true, false} {
			var out, errb bytes.Buffer
			step := len(tok) / pieces
			for k := 0; k < pieces; k++ {
				a, e := k*step, (k+1)*step
				if k == pieces-1 {
					e = len(tok)
				}
				frag := string(tok[a:e])
				toOut := (k%2 == 0) == startOut
				if toOut {
					out.Write(jsonLine(frag))
				} else {
					errb.WriteString(frag + "\n")
				}
			}
			r := b.Redact(out.Bytes(), errb.Bytes())
			if r.Blocked != "" {
				t.Fatalf("%d pieces: blocked %s", pieces, r.Blocked)
			}
			noGram(t, fmt.Sprint(pieces, "out"), tok, r.Stdout)
			noGram(t, fmt.Sprint(pieces, "err"), tok, r.Stderr)
		}
	}
}

// AC-15: a fragment straddling what would be a read-buffer boundary is
// irrelevant to an in-memory redaction, but the Normalizer is streaming:
// feeding in pieces must equal feeding at once.
func TestNormalizerStreamingEqualsBatch(t *testing.T) {
	src := []byte("a\\nb \\\\n c\\\td\\")
	for _, mode := range AllNorms {
		want := NormalizeBytes(mode, src)
		for cut := 0; cut <= len(src); cut++ {
			var got []byte
			n := Normalizer{Mode: mode}
			emit := func(b byte, _ int64) { got = append(got, b) }
			n.Feed(src[:cut], emit)
			n.Feed(src[cut:], emit)
			n.Flush(emit)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s cut %d: %q != %q", mode, cut, got, want)
			}
		}
	}
}

// AC-15: a hit at the start, middle and end of an integer, in a fraction and
// in an exponent blocks with redaction_numeric_hit, and leaves no hit in the
// copy.
func TestRedactNumericHit(t *testing.T) {
	digits := []byte("9081726354")
	tok := append(synthToken(t, 40), digits...)
	tok = append(tok, synthToken(t, 50)[11:]...)
	b := testBound(t, tok)
	for _, num := range []string{"9081726354", "11" + "90817263", "90817263" + "11", "4" + "9081726354" + "4",
		"0.9081726354", "1.5e90817263", "-90817263"} {
		line := []byte(`{"type":"result","n":` + num + `,"s":"ok"}` + "\n")
		r := b.Redact(line, nil)
		if r.Blocked != ClassNumericHit || r.Blocking == 0 {
			t.Fatalf("%s: blocked=%q blocking=%d", num, r.Blocked, r.Blocking)
		}
		if bytes.Contains(r.Stdout, []byte("90817263")) {
			t.Fatalf("%s: hit survives in the copy", num)
		}
	}
}

func TestRedactNoHitUnchanged(t *testing.T) {
	b := testBound(t, synthToken(t, 108))
	in := jsonLine("the test fails at lib.sh:42 because add returns 3")
	r := b.Redact(in, []byte("warning: nothing\n"))
	if !bytes.Equal(r.Stdout, in) || r.Rewrites != 0 || r.Blocked != "" || r.Exposed() {
		t.Fatalf("clean stream changed: %+v", r)
	}
}

func TestRedactJunkLineOpaque(t *testing.T) {
	tok := synthToken(t, 64)
	b := testBound(t, tok)
	in := append([]byte("not json "+string(tok)+"\n"), jsonLine("fine")...)
	r := b.Redact(in, nil)
	if bytes.Contains(r.Stdout, tok[:8]) || r.Blocked != "" {
		t.Fatalf("junk line: %q %s", r.Stdout, r.Blocked)
	}
}

func TestPrintingNeverShowsValue(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	j, _ := json.Marshal(struct {
		B *Bound
		S Secret
	}{b, b.token})
	for _, s := range []string{b.String(), fmt.Sprintf("%v %+v %#v", b, b.token, b.token), string(j)} {
		if strings.Contains(s, string(tok[:16])) || strings.Contains(s, string(tok[20:36])) {
			t.Fatalf("value printed: %s", s)
		}
	}
	_, err := NewBound(NewSecret(KindToken, "v0", tok[:31]))
	if err == nil || strings.Contains(err.Error(), string(tok[:31])) || !strings.Contains(err.Error(), "31") {
		t.Fatalf("length error: %v", err)
	}
	if _, err := NewBound(NewSecret(KindToken, "v0", append(tok, make([]byte, 513-len(tok))...))); err == nil {
		t.Fatal("513 bytes accepted")
	}
}
