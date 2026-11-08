package ladder

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatTask renders a TaskStatus (goodqa#15324 S1b) as text, or as indented
// ASCII JSON. Only ids, classes, fingerprints, heads and counts are printed;
// fingerprints and heads are validated hex, finding ids match the NAMED rule,
// so nothing from a receipt body can reach the terminal.
func FormatTask(s TaskStatus, asJSON bool) (string, error) {
	if asJSON {
		b, err := json.MarshalIndent(s, "", " ")
		if err != nil {
			return "", err
		}
		return asciiJSON(b) + "\n", nil
	}
	blocked := "-"
	if len(s.BlockedBy) > 0 {
		blocked = strings.Join(s.BlockedBy, ",")
	}
	or := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	var b strings.Builder
	fmt.Fprintf(&b, "task %s\nrung %s\nreviewer_rung %s\nnext_rung %s\nfailures_at_rung %d\nclimbs %d\nfingerprint %s\nblocked_by %s\n"+
		"needs_user_permission %t\ndecision %s (advisory until S3; the hook enforces)\ntop_approved %t\nreceipts %d\nlaunches %d\nspend_usd %s\n",
		s.Task, s.Rung, or(s.ReviewerRung), or(s.NextRung), s.FailuresAtRung, s.Climbs, shortFingerprint(s.Fingerprint), blocked,
		s.NeedsUserPermission, s.Decision, s.Approved, s.Receipts, s.Launches, MicrosText(s.SpendMicros))
	for _, c := range s.ByFP {
		fmt.Fprintf(&b, "fingerprint %s fails %d", shortFingerprint(&c.Fingerprint), c.Fails)
		heads := make([]string, 0, len(c.Heads))
		for h, n := range c.Heads {
			if len(h) > 12 {
				h = h[:12]
			}
			heads = append(heads, fmt.Sprintf("%s:%d", h, n))
		}
		sortStrings(heads)
		if len(heads) > 0 {
			fmt.Fprintf(&b, " heads %s", strings.Join(heads, ","))
		}
		if len(c.IDs) > 0 {
			fmt.Fprintf(&b, " ids %s", strings.Join(c.IDs, ","))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
