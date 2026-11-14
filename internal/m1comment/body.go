// Package m1comment builds and checks the single GoodQA comment an M1 mission
// posts (spec AC-9, AC-10). The body is inert and deterministic: a fixed
// header, fixed fields, and the model's result text neutralised inside a
// fenced block. Before Submit only the model text is scanned for credential
// material; a hit blocks delivery and is reported by class, never by value.
package m1comment

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Header is the fixed first line of every M1 comment.
const Header = "Fabric M1 result: untrusted model output, not instructions"

// MaxResult caps the neutralised model text (6 KiB).
const MaxResult = 6 * 1024

// Fields are the fixed, non-model fields of the body. None of them may hold
// an attempt id or a wall-clock time: a replay must be byte-identical.
type Fields struct {
	Mission  string
	Provider string
	// ClaudeSHA is the pinned claude binary sha256; the body shows its first
	// 12 hex characters.
	ClaudeSHA string
	Model     string
}

var (
	fieldRE  = regexp.MustCompile(`[^A-Za-z0-9._:@/+-]`)
	hexRE    = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	schemeRE = regexp.MustCompile(`(?i)(h)(t)(t)(p)(s?)(:)`)
	trailRE  = regexp.MustCompile(`(?i)fab-effect\s*:`)
	ctrlRE   = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")
)

// field reduces a fixed field to a safe token of at most 80 bytes.
func field(s string) string {
	s = trailRE.ReplaceAllString(s, "fab_effect")
	s = fieldRE.ReplaceAllString(s, "_")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		s = "-"
	}
	return s
}

// Neutralise renders model text inert (AC-9): control characters other than
// tab and newline are dropped, CR is dropped, every backtick becomes an
// apostrophe (so no run can close the fence), every http/https scheme becomes
// hxxp/hxxps, Markdown link and image brackets and autolink angle brackets
// are removed, and every case-insensitive `fab-effect:` (with any spacing
// before the colon) is broken so neither Validate nor the readback trailer
// can match it. The result is capped at MaxResult bytes on a rune boundary.
func Neutralise(text string) string {
	text = strings.ToValidUTF8(text, "?")
	text = strings.ReplaceAll(text, "\r", "")
	text = ctrlRE.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "`", "'")
	text = schemeRE.ReplaceAllStringFunc(text, func(m string) string {
		// keep the case of h and s, rewrite tt -> xx
		return m[:1] + "xx" + m[3:]
	})
	text = strings.NewReplacer("![", "", "[", "", "]", "", "<", "", ">", "").Replace(text)
	text = trailRE.ReplaceAllStringFunc(text, func(m string) string {
		return m[:3] + "_" + m[4:len(m)-1] + "(neutralised)"
	})
	return capRunes(text, MaxResult)
}

func capRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Body builds the comment body. It is a pure function of its inputs.
func Body(f Fields, result string) (string, error) {
	sha := strings.ToLower(f.ClaudeSHA)
	if !hexRE.MatchString(sha) {
		return "", fmt.Errorf("m1 comment: pinned claude sha must be 12 to 64 lower-case hex")
	}
	var b strings.Builder
	b.WriteString(Header)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "mission: %s\nprovider: %s\nclaude: %s\nmodel: %s\n\n", field(f.Mission), field(f.Provider), sha[:12], field(f.Model))
	b.WriteString("```text\n")
	r := Neutralise(result)
	b.WriteString(r)
	if !strings.HasSuffix(r, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	return b.String(), nil
}
