package codex_test

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fabric/internal/harness"
)

// TestRealCodex drives the real `codex app-server` through the adapter. It is
// opt in (FABRIC_CODEX_E2E=1) because it needs an installed, already signed-in
// codex and spends model tokens. The process gets only PATH and HOME from the
// caller; nothing is read or printed from codex's own credentials, and the
// test prints event kinds and counts, never event content.
func TestRealCodex(t *testing.T) {
	if os.Getenv("FABRIC_CODEX_E2E") != "1" {
		t.Skip("set FABRIC_CODEX_E2E=1 to run against the real codex")
	}
	t.Fatal("legacy real Codex test refused: use the reviewed controlled fab harness preflight/demo route; no unrestricted test fallback")
}

// summary counts events by kind, and unknown events by their classification
// (a method name or short reason, never harness content).
func summary(evs []harness.Event) string {
	kinds := map[harness.Kind]int{}
	whys := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
		if e.Kind == harness.KindUnknown {
			var u harness.Unknown
			if err := json.Unmarshal(e.Payload, &u); err == nil {
				whys[u.Why]++
			}
		}
	}
	var parts []string
	for k, n := range kinds {
		parts = append(parts, string(k)+"="+strconv.Itoa(n))
	}
	sort.Strings(parts)
	var ws []string
	for w, n := range whys {
		ws = append(ws, strconv.Itoa(n)+"x "+w)
	}
	sort.Strings(ws)
	return strings.Join(parts, " ") + " | unknown: " + strings.Join(ws, "; ")
}
