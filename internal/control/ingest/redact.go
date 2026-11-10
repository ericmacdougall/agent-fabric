package ingest

import (
	"regexp"
	"sort"
	"strings"
)

// A secret that reaches control.ingested_events stays there: the table is
// append-only (no UPDATE, DELETE or TRUNCATE). So every free-text field is
// redacted before it is stored, with the shapes of the harness's shared list
// (agent-harness d825d66 harness/hooks/redact.py) and the same placeholder,
// [REDACTED:<kind>]. Recall over precision: a false positive costs a
// placeholder in a summary, a false negative leaks a credential for good.
// Go's regexp is linear time, so hostile input cannot cause backtracking
// blowup; the bounded repeats of the original are loosened where RE2's
// 1000-repeat limit would not allow them.

const keysRE = `(?:[a-z0-9]+[_-])*(?:password|passwd|passphrase|secret|token|api[_-]?key|access[_-]?key|` +
	`private[_-]?key|credentials?|client[_-]?secret|refresh[_-]?token|access[_-]?token|id[_-]?token)(?:[_-]value)?`

type secretPattern struct {
	kind string
	re   *regexp.Regexp
}

var secretPatterns = []secretPattern{
	{"rclone-token", regexp.MustCompile(`(?im)^\s*token\s*=\s*\{[^\n]*`)},
	{"oauth-json", regexp.MustCompile(`(?is)\{[^{}]*?["'](?:access_token|refresh_token|id_token)["']\s*:[^{}]*\}`)},
	{"anthropic", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{8,}`)},
	{"openai", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{"github", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"aws-key-pair", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b[\s,:;="']{1,20}[A-Za-z0-9/+]{40}`)},
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[A-Z0-9]{16}\b`)},
	{"aws-secret", regexp.MustCompile(`(?i)\baws[_-]?secret[_-]?(?:access[_-]?)?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+]{40}`)},
	{"stripe", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{"stripe-webhook", regexp.MustCompile(`\bwhsec_[A-Za-z0-9]{16,}`)},
	{"huggingface", regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}`)},
	{"gitlab", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"sendgrid", regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)},
	{"google-oauth", regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}`)},
	{"google-api", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`)},
	{"google-refresh", regexp.MustCompile(`\b1//[A-Za-z0-9_-]{30,}`)},
	{"dropbox", regexp.MustCompile(`\bsl\.[A-Za-z0-9_-]{40,}`)},
	{"slack", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{6,}`)},
	{"npm", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{0,512}`)},
	{"bearer", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"url-userinfo", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{0,30}://[^\s/?#@]{1,256}@[^\s/?#]{1,256}`)},
	{"named-credential", regexp.MustCompile(`(?i)\b` + keysRE + `["']?\s*[:=]\s*["']?[^\s"',;}]{3,512}`)},
	{"named-credential", regexp.MustCompile(`\b[a-z][a-z0-9]*(?:Secret|Token|Password|ApiKey|AccessKey|PrivateKey|Credentials?)["']?\s*[:=]\s*["']?[^\s"',;}]{3,512}`)},
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}(?:PRIVATE KEY|ENCRYPTED PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]{0,40}-----`)
)

type span struct {
	start, end int
	kind       string
}

func findSecrets(s string) []span {
	var spans []span
	cursor := 0
	for {
		b := pemBegin.FindStringIndex(s[cursor:])
		if b == nil {
			break
		}
		start := cursor + b[0]
		stop := len(s)
		if e := pemEnd.FindStringIndex(s[cursor+b[1]:]); e != nil {
			stop = cursor + b[1] + e[1]
		}
		spans = append(spans, span{start, stop, "pem"})
		cursor = stop
	}
	for _, p := range secretPatterns {
		for _, m := range p.re.FindAllStringIndex(s, -1) {
			if strings.Contains(s[m[0]:m[1]], "[REDACTED:") {
				continue
			}
			spans = append(spans, span{m[0], m[1], p.kind})
		}
	}
	return spans
}

// Redact replaces every secret-shaped span with [REDACTED:<kind>] and reports
// how many spans it replaced. It is idempotent. An overlapping span is swallowed
// by the outer one, so no fragment of a secret survives.
func Redact(s string) (string, int) {
	spans := findSecrets(s)
	if len(spans) == 0 {
		return s, 0
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end-spans[i].start > spans[j].end-spans[j].start
	})
	var b strings.Builder
	cursor, n := 0, 0
	for _, sp := range spans {
		if sp.start < cursor {
			if sp.end > cursor {
				cursor = sp.end
			}
			continue
		}
		b.WriteString(s[cursor:sp.start])
		b.WriteString("[REDACTED:" + sp.kind + "]")
		cursor = sp.end
		n++
	}
	b.WriteString(s[cursor:])
	return b.String(), n
}

// redactEvent replaces secret-shaped spans in every free-text value before the
// event can be stored (the table is append-only). A redacted summary is cut back
// to the 280-character limit.
func redactEvent(ev *Event) {
	do := func(s *string) {
		if r, n := Redact(*s); n > 0 {
			*s = r
			ev.Redacted++
		}
	}
	do(&ev.Summary)
	do(&ev.Host)
	do(&ev.Tenant)
	for _, m := range []map[string]string{ev.Actor, ev.Subject} {
		for k, v := range m {
			if r, n := Redact(v); n > 0 {
				m[k] = r
				ev.Redacted++
			}
		}
	}
	ev.Summary = cut(ev.Summary, MaxSummary)
}

// hasSecret reports whether s contains a secret-shaped span (used for the id,
// which is never rewritten: a line whose id looks like a credential is refused).
func hasSecret(s string) bool { return len(findSecrets(s)) > 0 }
