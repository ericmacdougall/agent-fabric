package main

import "testing"

// Only the exact value "1" turns the review gate on; anything else keeps native
// runs exactly as they were without it.
func TestReviewGateFlagIsOffUnlessExactlyOne(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "true": false, "yes": false, " 1": false, "1 ": false, "01": false, "1": true} {
		t.Setenv(reviewGateFlag, value)
		if got := reviewGateEnabled(); got != want {
			t.Errorf("%s=%q: enabled=%v, want %v", reviewGateFlag, value, got, want)
		}
	}
}
