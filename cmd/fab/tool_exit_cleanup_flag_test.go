package main

import "testing"

// The flag is opt-in: unset and every value but "1" keep main's behaviour.
func TestToolExitCleanupFlagIsExactOptIn(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", false}, {"0", false}, {"true", false}, {" 1", false}, {"1", true}} {
		t.Setenv(toolExitCleanupFlag, tc.value)
		if got := toolExitCleanupEnabled(); got != tc.want {
			t.Errorf("%s=%q: enabled=%v, want %v", toolExitCleanupFlag, tc.value, got, tc.want)
		}
	}
}
