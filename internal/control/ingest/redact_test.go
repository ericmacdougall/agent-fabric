package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fake credentials are assembled at run time so no secret-shaped literal sits in the source.
func fakes() map[string]string {
	a := func(n int) string { return strings.Repeat("A", n) }
	return map[string]string{
		"github":           "ghp_" + a(36),
		"openai":           "sk-" + a(30),
		"anthropic":        "sk-ant-" + a(20),
		"aws-key":          "AKIA" + strings.Repeat("B", 16),
		"slack":            "xoxb-" + a(12),
		"stripe":           "sk_live_" + a(20),
		"jwt":              "eyJ" + a(12) + ".eyJ" + a(12) + "." + a(12),
		"bearer":           "Bearer " + a(24),
		"url-userinfo":     "https://user:pa55word@example.com/path",
		"named-credential": "password=hunter2hunter2",
		"pem":              "-----BEGIN RSA PRIVATE KEY-----\n" + a(40) + "\n-----END RSA PRIVATE KEY-----",
	}
}

// A secret in free text is replaced before storage (the table is append-only).
func TestRedactReplacesSecrets(t *testing.T) {
	for kind, secret := range fakes() {
		got, n := Redact("before " + secret + " after")
		if n == 0 || strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED:") || !strings.HasPrefix(got, "before ") {
			t.Errorf("%s: n=%d got %q", kind, n, got)
		}
		// no fragment of the secret body survives
		if frag := strings.TrimLeft(secret, "-"); len(frag) > 20 && strings.Contains(got, frag[len(frag)-16:]) {
			t.Errorf("%s: a fragment survived: %q", kind, got)
		}
		again, n2 := Redact(got)
		if again != got || n2 != 0 {
			t.Errorf("%s: not idempotent: %q -> %q", kind, got, again)
		}
	}
	for _, benign := range []string{"deployed fabric to t2-dev", "goodqa#15586 ready for review", "tokens used: 12", "the password reset page"} {
		if got, n := Redact(benign); got != benign || n != 0 {
			t.Errorf("benign text changed: %q -> %q", benign, got)
		}
	}
}

// Every free-text field of a validated event is redacted, and the count is reported.
func TestValidateRedactsEveryTextField(t *testing.T) {
	f := fakes()
	line := `{"schema":"mc.event/1","id":"wal-x:r1","ts":"2026-11-09T12:30:45Z","kind":"wal","host":"h ` + f["github"] + `","tenant":"t ` + f["openai"] +
		`","actor":{"agent":"a ` + f["slack"] + `"},"subject":{"ref":"r ` + f["aws-key"] + `","repo":"` + f["url-userinfo"] + `"},"summary":"leak ` + f["github"] + `"}`
	ev, err := ValidateLine([]byte(line), "wal-x")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join([]string{ev.Host, ev.Tenant, ev.Actor["agent"], ev.Subject["ref"], ev.Subject["repo"], ev.Summary}, "|")
	for kind, secret := range f {
		if kind == "github" || kind == "openai" || kind == "slack" || kind == "aws-key" || kind == "url-userinfo" {
			if strings.Contains(all, secret) {
				t.Errorf("%s survived in %q", kind, all)
			}
		}
	}
	if ev.Redacted != 6 {
		t.Errorf("redacted fields = %d, want 6 (%q)", ev.Redacted, all)
	}
}

// A line whose id looks like a credential is refused, never rewritten.
func TestSecretIDRefused(t *testing.T) {
	_, err := ValidateLine([]byte(`{"schema":"mc.event/1","id":"wal-x:`+fakes()["github"]+`","ts":"2026-11-09T12:30:45Z","kind":"wal"}`), "wal-x")
	if err == nil || err.Error() != "secret-in-id" {
		t.Fatalf("got %v", err)
	}
}

// A redacted summary that grew past 280 is cut back, and 1 MiB of near-misses stays fast (linear matching).
func TestRedactBounds(t *testing.T) {
	sum := strings.Repeat("sk-", 90) + " " + fakes()["github"] // 270 chars + token
	if len(sum) > MaxSummary {
		sum = sum[:MaxSummary]
	}
	line := `{"schema":"mc.event/1","id":"wal-x:b","ts":"2026-11-09T12:30:45Z","kind":"wal","summary":"` + strings.Repeat("a", 100) + fakes()["github"][:30] + `"}`
	if ev, err := ValidateLine([]byte(line), "wal-x"); err != nil || len([]rune(ev.Summary)) > MaxSummary {
		t.Fatalf("%v %d", err, len([]rune(ev.Summary)))
	}
	hostile := strings.Repeat("password=", 4000) + strings.Repeat("x", 20000)
	if _, n := Redact(hostile); n == 0 {
		t.Fatal("hostile input not matched")
	}
}

// RE2's \s is ASCII-only while the harness's Python redact.py treats every
// Unicode space as \s. A credential whose key and value are separated by a
// no-break or typographic space (macOS option-space, &nbsp; pasted from HTML,
// French "mot de passe : x") must still be redacted before the append-only
// insert. The strings are assembled from code points, with fake values.
func TestRedactUnicodeSpaces(t *testing.T) {
	spaces := []rune{0x00A0, 0x1680, 0x2000, 0x2005, 0x200A, 0x202F, 0x205F, 0x3000}
	awsSecret := "wJalrXUtnFEMI" + strings.Repeat("K7MDENGbPx", 2) + "RfiCYEX" // 40 chars, fake
	if len(awsSecret) != 40 {
		t.Fatalf("fake aws secret is %d chars", len(awsSecret))
	}
	akia := "AKIA" + strings.Repeat("Q", 16)
	for _, r := range spaces {
		sp := string(r)
		for _, c := range []struct{ in, value string }{
			{"password" + sp + "=" + sp + "hunter2xyz", "hunter2xyz"},
			{"Authorization: Bearer" + sp + "abcdef0123456789", "abcdef0123456789"},
			{"api_key" + sp + ":" + sp + "k3yv4lu3abc", "k3yv4lu3abc"},
			{"aws_secret_access_key" + sp + "=" + sp + awsSecret, awsSecret},
			{"clientSecret" + sp + "=" + sp + "'k3yv4lu3abc'", "k3yv4lu3abc"},
			{akia + sp + awsSecret, awsSecret},
		} {
			got, n := Redact("note " + c.in + " end")
			if n == 0 || strings.Contains(got, c.value[len(c.value)-8:]) {
				t.Errorf("U+%04X: %q -> n=%d %q", r, c.in, n, got)
			}
			if again, n2 := Redact(got); again != got || n2 != 0 {
				t.Errorf("U+%04X: not idempotent: %q -> %q", r, got, again)
			}
		}
		// end to end: the stored summary carries no secret
		line, _ := json.Marshal(map[string]string{"schema": "mc.event/1", "id": "wal-x:zs", "ts": "2026-11-09T12:30:45Z", "kind": "wal",
			"summary": "password" + sp + "=" + sp + "hunter2xyz"})
		ev, err := ValidateLine(line, "wal-x")
		if err != nil || ev.Redacted != 1 || strings.Contains(ev.Summary, "hunter2xyz") {
			t.Errorf("U+%04X: ValidateLine err=%v redacted=%d summary=%q", r, err, ev.Redacted, ev.Summary)
		}
	}
	// benign prose with no-break spaces is left alone
	benign := "deployed fabric to t2-dev : ok"
	if got, n := Redact(benign); got != benign || n != 0 {
		t.Errorf("benign text changed: %q -> %q", benign, got)
	}
}
