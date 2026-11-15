package m1comment

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"fabric/internal/credential"
)

// The integration seam (goodqa#15966 W2 x W3): the pre-submit scan runs
// through W3's real matcher, not the synthetic one the W2 tests use.
var _ BoundCredentials = (*credential.Bound)(nil)

func realBound(t *testing.T, token string) *credential.Bound {
	t.Helper()
	b, err := credential.NewBound(credential.NewSecret(credential.KindToken, "claude-oauth.v0", []byte(token)),
		credential.NewSecret(credential.KindGoodQA, "goodqa", []byte(gqKey)),
		credential.NewSecret(credential.KindDSN, "runner", []byte(dsnPW)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// AC-10 must-pass fixtures through the real type: the canary diagnosis and
// the allowed-path texts, whose Neutralise form is scanned a second time.
func TestRealBoundPassesDiagnosisAndAllowedPaths(t *testing.T) {
	wt := "/srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/"
	texts := []string{
		`The failing test is test.sh, which calls sum_to from lib.sh.
In /srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/lib.sh line 7 the loop runs
"for ((i = 1; i < n; i++))", so sum_to 5 returns 10 instead of 15: an off-by-one.
/srv/fabric/runs/r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3/wt/test.sh lines 4-9 assert sum_to 1, 5 and 10.
Fix: use i <= n. Mission m_canary-m1, attempt r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3,
commit 8297e422ad519e23152e7a21119ed9bc5c8991ad. Output: FAIL sum_to 5: want 15, got 10.`,
		"Root cause: [lib.sh](" + wt + "lib.sh) sum_to uses -lt, an off-by-one; see <" + wt + "test.sh>.",
		"attempt [r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3] ran ./test.sh, exit 1 in [" + wt + "lib.sh]",
		"![trace](" + wt + "test.sh) mission [m_canary-m1] <r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3>",
		"exit1[r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3]failed and run<r_01JD8XK2Q7M4N6P8R0S2T4V6W8Y0Z1A3>ended",
	}
	for seed := int64(10); seed < 60; seed++ {
		b := realBound(t, synthToken(seed))
		for i, text := range texts {
			if blocked, class := Scan(text, b, allow); blocked {
				t.Fatalf("seed %d text %d blocked through credential.Bound: %s", seed, i, class)
			}
		}
	}
}

// AC-10 block fixtures through the real type: every class, the bound
// version after rotation, and 3-, 5- and 7-byte fragments.
func TestRealBoundBlocksFixtures(t *testing.T) {
	b := realBound(t, tokA)
	body := tokA[13:]
	cases := map[string]string{
		"raw token":      "the env had " + tokA,
		"base64 token":   "b64: " + base64.StdEncoding.EncodeToString([]byte(tokA)),
		"hex token":      hex.EncodeToString([]byte(tokA)),
		"goodqa key":     "key " + gqKey,
		"dsn password":   "pw=" + dsnPW,
		"sk-ant literal": "looks like sk-ant-api03 prefix",
		"postgres url":   "connect to postgres://u@h/db",
		"entropy run":    "blob Zx9Qw2Lk8Vb3Np7Rt5Ym1Hc4Jd6Fg0SaUeIoPyTrEwQ end",
		"secret split":   "value " + strings.Join([]string{gqKey[:5], gqKey[5:10], gqKey[10:]}, "]"),
	}
	for _, n := range []int{3, 5, 7} {
		var parts []string
		for i := 0; i < len(body); i += n {
			parts = append(parts, body[i:min(i+n, len(body))])
		}
		js, _ := json.Marshal(parts)
		cases["punctuation "+string(rune('0'+n))] = strings.Join(parts, ". ")
		cases["json "+string(rune('0'+n))] = string(js)
		cases["markdown "+string(rune('0'+n))] = "* " + strings.Join(parts, "\n* ")
	}
	for name, text := range cases {
		blocked, class := Scan(text, b, allow)
		if !blocked {
			t.Errorf("%s: not blocked through credential.Bound", name)
		}
		if strings.Contains(class, body[:8]) || strings.Contains(class, gqKey[8:16]) {
			t.Errorf("%s: class carries credential bytes", name)
		}
	}
	// Bound to A, rotated to B before settle: base64 of A still blocks.
	if blocked, _ := Scan("stale "+base64.StdEncoding.EncodeToString([]byte(tokA)), realBound(t, tokA), allow); !blocked {
		t.Error("base64 of the bound version A not blocked")
	}
}
