package ingest

import (
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
