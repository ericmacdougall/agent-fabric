package harness

import "strings"

// MinLiteralLen is the shortest value LiteralRedactor will mask: shorter
// values would mask ordinary text.
const MinLiteralLen = 8

type literalRedactor []string

// LiteralRedactor masks every occurrence of the given values, whatever their
// shape. Callers use it for credentials they hand to a child (passed
// environment values), which the shape-based PatternRedactor cannot know.
func LiteralRedactor(values []string) Redactor {
	var out literalRedactor
	for _, v := range values {
		if len(v) >= MinLiteralLen {
			out = append(out, v)
		}
	}
	return out
}

func (l literalRedactor) Redact(s string) string {
	for _, v := range l {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}
