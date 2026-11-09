package harness

// ChainRedactor applies each redactor in order, so a caller's own redactor
// (for example the one that knows a run's API keys) and the pattern redactor
// both cover every string that leaves an adapter.
func ChainRedactor(rs ...Redactor) Redactor { return chain(rs) }

type chain []Redactor

func (c chain) Redact(s string) string {
	for _, r := range c {
		s = r.Redact(s)
	}
	return s
}
