// Package policy holds the always-on guard subset for native tool calls
// (slice 1): secret redaction at tool-result creation (D9) and an
// out-of-process oracle that asks the harness's Python PreToolUse hooks
// whether a shell command is a secret dump, log deletion, production
// delete or broad kill. The Go port of those rules is slice 4.
package policy

import (
	"regexp"
	"strings"
)

type shape struct {
	kind string
	re   *regexp.Regexp
}

// The shape list mirrors ~/.claude/hooks/redact.py. RE2 caps a counted
// repeat at 1000, so the long bounded repeats there are shortened; recall
// matters more than precision (a placeholder costs nothing, a leaked
// credential in a stored transcript is permanent).
const keys = `(?:[a-z0-9]+[_-])*(?:password|passwd|passphrase|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|client[_-]?secret|refresh[_-]?token|access[_-]?token|id[_-]?token)(?:[_-]value)?`

var shapes = []shape{
	{"rclone-token", regexp.MustCompile(`(?im)^\s*token\s*=\s*\{[^\n]{0,1000}`)},
	{"oauth-json", regexp.MustCompile(`(?is)\{[^{}]{0,1000}?["'](?:access_token|refresh_token|id_token)["']\s*:[^{}]{0,1000}\}`)},
	{"anthropic", regexp.MustCompile(`\bsk-` + `ant-[A-Za-z0-9_-]{8,}`)},
	{"openai", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{"github", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"aws-key-pair", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b[\s,:;="']{1,20}[A-Za-z0-9/+]{40}`)},
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[A-Z0-9]{16}\b`)},
	{"aws-secret", regexp.MustCompile(`(?i)\baws[_-]?secret[_-]?(?:access[_-]?)?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+]{40}`)},
	{"google-oauth", regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`)},
	{"google-api", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`)},
	{"google-refresh", regexp.MustCompile(`\b1//[A-Za-z0-9_-]{30,}`)},
	{"dropbox", regexp.MustCompile(`\bsl\.[A-Za-z0-9_-]{40,}`)},
	{"slack", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{6,}`)},
	{"npm", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{0,512}`)},
	{"bearer", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"url-userinfo", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/?#@]{1,256}@[^\s/?#]{1,256}`)},
	{"named-credential", regexp.MustCompile(`(?i)\b` + keys + `["']?\s*[:=]\s*["']?[^\s"',;}]{3,512}`)},
	{"named-credential", regexp.MustCompile(`\b[a-z][a-z0-9]*(?:Secret|Token|Password|ApiKey|AccessKey|PrivateKey|Credentials?)["']?\s*[:=]\s*["']?[^\s"',;}]{3,512}`)},
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}(?:PRIVATE KEY|ENCRYPTED PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]{0,40}-----`)
)

// Redactor replaces secret-shaped spans and any registered literal (such as
// a key this process holds) with [REDACTED:kind].
type Redactor struct {
	// Literals are exact secrets known to this process; they are removed
	// even when they match no shape.
	Literals []string
}

func (r Redactor) Redact(s string) string {
	for _, l := range r.Literals {
		if len(l) >= 8 {
			s = strings.ReplaceAll(s, l, "[REDACTED:literal]")
		}
	}
	// PEM bodies are unbounded: handle them before the bounded shapes.
	for {
		loc := pemBegin.FindStringIndex(s)
		if loc == nil {
			break
		}
		stop := len(s)
		if e := pemEnd.FindStringIndex(s[loc[1]:]); e != nil {
			stop = loc[1] + e[1]
		}
		s = s[:loc[0]] + "[REDACTED:pem]" + s[stop:]
	}
	for _, sh := range shapes {
		s = sh.re.ReplaceAllString(s, "[REDACTED:"+sh.kind+"]")
	}
	return s
}

// Find reports the kinds present without returning values.
func Find(s string) []string {
	var kinds []string
	if pemBegin.MatchString(s) {
		kinds = append(kinds, "pem")
	}
	for _, sh := range shapes {
		if sh.re.MatchString(s) {
			kinds = append(kinds, sh.kind)
		}
	}
	return kinds
}
