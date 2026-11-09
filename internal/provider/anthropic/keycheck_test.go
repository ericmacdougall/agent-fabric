package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

type authTransport struct {
	status int
	calls  int
	got    *http.Request
}

func (a *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	a.calls++
	a.got = r
	h := http.Header{}
	if a.status >= 300 && a.status < 400 {
		h.Set("Location", "https://example.invalid/steal")
	}
	return &http.Response{StatusCode: a.status, Body: http.NoBody, Header: h, Request: r}, nil
}

// A key whose stored shape is not the documented one (here: not sk-ant-api)
// is accepted only in authority mode and only if the API answers 200.
func TestKeyCheckAuthority(t *testing.T) {
	odd := "sk-ant-zzz-" + strings.Repeat("aB3_-", 18)
	oat := "sk-ant-oat01-" + strings.Repeat("aB3_-", 18)
	cases := []struct {
		name   string
		key    string
		mode   KeyCheck
		status int
		ok     bool
		calls  int
	}{
		{"default refuses a bad shape without a network call", odd, "", 200, false, 0},
		{"shape mode refuses a bad shape", odd, KeyCheckShape, 200, false, 0},
		{"authority refuses an OAuth token without a network call", oat, KeyCheckAuthority, 200, false, 0},
		{"authority refuses OAuth in any case", strings.ToUpper(oat[:12]) + oat[12:], KeyCheckAuthority, 200, false, 0},
		{"authority refuses a value that is unsafe in a header", odd[:20] + "\x7f" + odd[20:], KeyCheckAuthority, 200, false, 0},
		{"authority refuses a GitHub token", "ghp_" + strings.Repeat("aB3", 14), KeyCheckAuthority, 200, false, 0},
		{"authority refuses an OpenAI-style key", "sk-proj-" + strings.Repeat("aB3", 14), KeyCheckAuthority, 200, false, 0},
		{"authority refuses an admin key", "sk-ant-admin01-" + strings.Repeat("aB3_-", 18), KeyCheckAuthority, 200, false, 0},
		{"authority refuses a refresh token", "sk-ant-ort01-" + strings.Repeat("aB3_-", 18), KeyCheckAuthority, 200, false, 0},
		{"authority refuses a session token", "sk-ant-sid01-" + strings.Repeat("aB3_-", 18), KeyCheckAuthority, 200, false, 0},
		{"authority refuses a prefix trick", "x" + oat, KeyCheckAuthority, 200, false, 0},
		{"authority refuses a new value on a 503", odd, KeyCheckAuthority, 503, false, 1},
		{"authority refuses a new value on a 429", odd, KeyCheckAuthority, 429, false, 1},
		{"authority refuses a 401", odd, KeyCheckAuthority, 401, false, 1},
		{"authority refuses a 403", odd, KeyCheckAuthority, 403, false, 1},
		{"authority refuses a redirect", odd, KeyCheckAuthority, 302, false, 1},
		{"authority accepts a 200", odd, KeyCheckAuthority, 200, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &authTransport{status: c.status}
			var status int
			var prefix string
			src, err := KeyFromCommandWith([]string{"printf", "%s", c.key + "\n"}, time.Minute, nil, KeyOptions{
				Check: c.mode, HTTP: &http.Client{Transport: tr},
				OnCheck: func(s int, p string) { status, prefix = s, p },
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := src(context.Background())
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v err=%v", c.ok, err)
			}
			if c.ok && got != c.key {
				t.Fatal("accepted key differs")
			}
			if !c.ok && got != "" {
				t.Fatal("a refused key was returned")
			}
			if tr.calls != c.calls {
				t.Fatalf("%d network calls, want %d", tr.calls, c.calls)
			}
			if err != nil && (strings.Contains(err.Error(), c.key) || strings.Contains(err.Error(), c.key[:20])) {
				t.Fatal("the error names the key")
			}
			if c.calls == 1 {
				sum := sha256.Sum256([]byte(c.key))
				if status != c.status || prefix != hex.EncodeToString(sum[:])[:8] {
					t.Fatalf("recorded %d %q", status, prefix)
				}
				if tr.got.Method != http.MethodGet || tr.got.URL.String() != "https://api.anthropic.com/v1/models?limit=1" || tr.got.Header.Get("x-api-key") != c.key {
					t.Fatalf("unexpected authority request %s %s", tr.got.Method, tr.got.URL)
				}
			}
		})
	}
}

// A shaped key never causes an authority request, in either mode.
func TestKeyCheckAuthoritySkippedForShapedKey(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("aB3_-", 18)
	tr := &authTransport{status: 200}
	src, _ := KeyFromCommandWith([]string{"printf", "%s", key}, time.Minute, nil, KeyOptions{Check: KeyCheckAuthority, HTTP: &http.Client{Transport: tr}})
	if _, err := src(context.Background()); err != nil || tr.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, tr.calls)
	}
}

func TestKeyCheckValid(t *testing.T) {
	for k, want := range map[KeyCheck]bool{"": true, "shape": true, "authority": true, "none": false, "Authority": false} {
		if k.Valid() != want {
			t.Errorf("Valid(%q) != %v", k, want)
		}
	}
}
