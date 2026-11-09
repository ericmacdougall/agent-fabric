package harnesstest

import (
	"testing"

	"fabric/internal/harness"
)

// Optional can waive only the kinds a harness may legitimately never report.
// Turn and message events are the contract; waiving them would let an adapter
// with no output pass the conformance suite.
func TestOptionalCanWaiveOnlyUsageAndToolCalls(t *testing.T) {
	all := []harness.Kind{harness.KindTurn, harness.KindToolCall, harness.KindMessage, harness.KindUsage, harness.KindFinal, harness.KindUnknown}
	for _, k := range all {
		want := k == harness.KindUsage || k == harness.KindToolCall
		if got := optional(all, k); got != want {
			t.Errorf("optional(all, %s) = %v, want %v", k, got, want)
		}
	}
	if optional(nil, harness.KindUsage) {
		t.Error("nothing waived when the list is empty")
	}
}
