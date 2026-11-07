package main

import "testing"

// goodqa#15086: the cap gauges fab metrics and the TUI export are the
// binary's fixed ceilings, month USD 20, job USD 5, live test USD 1.
func TestSpendCapsWiring(t *testing.T) {
	want := map[string]int64{"month": 20_000_000, "job": 5_000_000, "live_test": 1_000_000}
	got := spendCaps()
	if len(got) != len(want) {
		t.Fatalf("caps %+v, want %v", got, want)
	}
	for _, c := range got {
		if w, ok := want[c.Scope]; !ok || c.Micros != w {
			t.Errorf("cap %s = %d micros, want %d", c.Scope, c.Micros, w)
		}
	}
}
