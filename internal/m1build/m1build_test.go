package m1build

import "testing"

// The tag decides the constant; run with and without -tags m1.
func TestNameFollowsTag(t *testing.T) {
	want := map[bool]string{true: "m1", false: "non-m1"}[Enabled]
	if got := Name(); got != want {
		t.Fatalf("Name() = %q with Enabled=%v, want %q", got, Enabled, want)
	}
}
