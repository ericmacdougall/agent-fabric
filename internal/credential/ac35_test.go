package credential

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// AC-35(b), spec v16: the exposure boundary is 8 secret bytes (the bytes
// after the public prefix) in the encoding at hand. Each pair is one short
// of the boundary and exactly at it: the public prefix plus 7 or 8 raw
// characters, 15 or 16 hex characters of the secret part, and 10 or 11
// base64 characters starting at a decoded byte offset of 13 or later.
// Every member blocks the comment (AC-10) and is redacted; only the second
// of each pair is a credential exposure.
func TestSecretByteBoundaryPairs(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	pre := PublicPrefix(tok)
	if len(pre) != 13 {
		t.Fatalf("fixture: public prefix is %d bytes", len(pre))
	}
	secret := tok[len(pre):]
	hx := hex.EncodeToString(secret)
	b64 := base64.StdEncoding.EncodeToString(tok)
	const at = 20 // base64 character 20 starts decoded byte 15 (>= 13)
	pairs := []struct {
		name         string
		short, exact string
	}{
		{"raw", string(pre) + string(secret[:7]), string(pre) + string(secret[:8])},
		{"hex", hx[:15], hx[:16]},
		{"base64", b64[at : at+10], b64[at : at+11]},
	}
	for _, p := range pairs {
		for i, s := range []string{p.short, p.exact} {
			want := i == 1
			// No alphanumeric context: the alnum-stripped view would join a
			// neighbouring letter to the window, and when it equals the next
			// secret character the short member really is 8 secret bytes.
			text := s
			hit, class := b.ScanText(text)
			if !hit {
				t.Errorf("%s/%d: the comment is not blocked", p.name, len(s))
			}
			if (class == ClassFragment) != want {
				t.Errorf("%s/%d: scan class %q, exposure want %v", p.name, len(s), class, want)
			}
			r := b.Redact([]byte(`{"_":"`+text+`"}`+"\n"), nil)
			if r.Exposed() != want {
				t.Errorf("%s/%d: redaction exposure %v, want %v (labels %v)", p.name, len(s), r.Exposed(), want, SortedLabels(r.Counts))
			}
		}
	}
}
