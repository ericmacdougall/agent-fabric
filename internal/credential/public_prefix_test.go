package credential

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// Owner ruling 2026-11-14 (agent ad1ff08b576984a4d): a fragment that matches
// only the token's public prefix is the public literal, counted under
// LabelLiteral, never credential_exposure, and still blocks a comment
// (AC-10, sk_ant_literal). A fragment of 8 or more bytes from the secret
// part is an exposure.
func TestPublicPrefixIsNotAnExposure(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	pre := PublicPrefix(tok)
	if string(pre) != "sk-"+"ant-oat01-" {
		t.Fatalf("public prefix length %d", len(pre))
	}
	for name, text := range map[string]string{
		"prefix alone":          string(pre),
		"prefix without sk-":    "out := []byte(\"sk-\" + \"" + string(pre[3:]) + "\")",
		"scanner regexp source": "regexp.MustCompile(`^" + string(pre) + "[A-Za-z0-9_-]{67,499}$`)",
		// The corpus-2 case at ff56c15: prose naming the prefix, then a word
		// that N_ws joins to it. Here the word starts with the first 7 secret
		// bytes: a straddle with fewer than 8 secret bytes is the literal.
		// ')' ends the run: it appears in no token form.
		"prefix, space, 7 secret bytes": "the prefix (" + string(pre) + " " + string(tok[len(pre):len(pre)+7]) + ") for the AC-17 token",
	} {
		r := b.Redact(jsonLine(text), []byte(text+"\n"))
		if r.Exposed() || r.Counts[LabelLiteral] == 0 || r.Counts[b.tokLabel+":"+LabelFragment] != 0 {
			t.Errorf("%s: exposed=%v labels=%v", name, r.Exposed(), SortedLabels(r.Counts))
		}
		if hit, class := b.ScanText(text); !hit || class != ClassSkAnt {
			t.Errorf("%s: comment scan hit=%v class=%q, want blocked as %s", name, hit, class, ClassSkAnt)
		}
	}
}

func TestSecretFragmentIsAnExposure(t *testing.T) {
	// The secret part starts after 20 random bytes with a punctuated run:
	// "Ab-cD_eF" has only 6 alphanumerics (the t2-dev failure of 6952bcc).
	tok := append(synthToken(t, 33), []byte("Ab-cD_eF")...)
	tok = append(tok, synthToken(t, 80)[13:]...)
	b := testBound(t, tok)
	secret := tok[len(PublicPrefix(tok)):]
	for name, text := range map[string]string{
		"8 secret bytes":             "note " + string(secret[20:28]) + " end",
		"prefix then 8 secret":       "x " + string(tok[:len(tok)-len(secret)+8]) + " y",
		"unbound token after sk-ant": "v0 was " + string(synthToken(t, 108)),
	} {
		r := b.Redact(jsonLine(text), nil)
		if !r.Exposed() {
			t.Errorf("%s: not an exposure: %v", name, SortedLabels(r.Counts))
		}
		if bytes.Contains(r.Stdout, secret[20:28]) {
			t.Errorf("%s: secret bytes survive", name)
		}
		if hit, _ := b.ScanText(text); !hit {
			t.Errorf("%s: comment scan passed", name)
		}
	}
	// The fragment case is labelled as the token's fragment.
	r := b.Redact(jsonLine("note "+string(secret[20:28])+" end"), nil)
	if r.Counts[b.tokLabel+":"+LabelFragment] == 0 {
		t.Fatalf("labels %v", SortedLabels(r.Counts))
	}
	if hit, class := b.ScanText("note " + string(secret[20:28]) + " end"); !hit || class != ClassFragment {
		t.Fatalf("secret fragment class %q", class)
	}
}

// goodqa#16040 corpus 2 at b19789e: a random hex id that shares one
// 8-character window (4 bytes) with the token's hex form is not an
// exposure; 8 secret bytes in hex (16 characters) are. The id is still
// rewritten where it matches, and it never blocks.
func TestHexWindowNeedsEightSecretBytes(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	secret := tok[len(PublicPrefix(tok)):]
	hx := []byte(hex.EncodeToString(secret))
	for name, c := range map[string]struct {
		text    string
		exposed bool
	}{
		"4 secret bytes inside a hex id": {"\"id\":\"toolu_1e5cf454" + string(hx[40:48]) + "c8a869c8\"", false},
		"7 secret bytes in hex":          {"sum " + string(hx[40:54]) + " ok", false},
		"8 secret bytes in hex":          {"sum " + string(hx[40:56]) + " ok", true},
		"8 secret bytes in HEX":          {"sum " + strings.ToUpper(string(hx[40:56])) + " ok", true},
	} {
		r := b.Redact(jsonLine(c.text), nil)
		if r.Exposed() != c.exposed || r.Blocked != "" {
			t.Errorf("%s: exposed=%v blocked=%q labels=%v", name, r.Exposed(), r.Blocked, SortedLabels(r.Counts))
		}
		if hit, class := b.ScanText(c.text); !hit || (class == ClassFragment) != c.exposed {
			t.Errorf("%s: comment scan hit=%v class=%q", name, hit, class)
		}
	}
}
