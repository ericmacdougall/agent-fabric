package credential

import "strings"

// ScanUnbound is the AC-10 scan with no bound values: the checks that need
// no credential (the sk-ant literal, a postgres DSN, a high-entropy run).
// It is for operator free text written into a record (goodqa#15966 AC-34
// release reason), which must never carry a credential. The text never
// leaves this function.
func ScanUnbound(text string) (hit bool, class string) {
	if strings.Contains(text, PublicLiteral) {
		return true, ClassSkAnt
	}
	low := strings.ToLower(text)
	if strings.Contains(low, "postgres://") || strings.Contains(low, "postgresql://") {
		return true, ClassPostgres
	}
	if highEntropyRun(text, nil) {
		return true, ClassEntropy
	}
	return false, ""
}
