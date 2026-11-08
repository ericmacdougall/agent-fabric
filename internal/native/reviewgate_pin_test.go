package native

import (
	"strings"
	"testing"
)

// The resume pin of a flag-off run must stay byte-identical to a build without
// the gate, so attempts started before the gate existed can still resume. With
// the gate on, its mode and safe tools are pinned.
func TestReviewGatePinIsUnchangedWhenOff(t *testing.T) {
	g := Grant{ShellAllow: []string{"^ls$"}, WallSeconds: 60, MaxTurns: 4}
	off := g.pin(false)
	if strings.Contains(off, "Review") {
		t.Fatalf("flag-off pin mentions the gate: %s", off)
	}
	on := g.pin(true)
	if !strings.Contains(on, `"Review"`) || on == off {
		t.Fatalf("flag-on pin does not pin the gate: %s", on)
	}
	g.ReviewGate = ReviewGateConfig{Mode: "enforce"}
	if g.pin(true) == on {
		t.Fatal("changing the gate mode did not change the flag-on pin")
	}
	if g.pin(false) != off {
		t.Fatal("grant gate settings leaked into the flag-off pin")
	}
}
