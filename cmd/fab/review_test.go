package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"fabric/internal/reviewgate"
)

func TestReviewOperatorActor(t *testing.T) {
	for _, actor := range []string{"", "chat", "op:", "op:two people", "agent:one"} {
		t.Setenv("FABRIC_ACTOR", actor)
		if _, err := reviewOperatorActor(); err == nil {
			t.Errorf("actor %q accepted", actor)
		}
	}
	t.Setenv("FABRIC_ACTOR", "op:alice")
	if actor, err := reviewOperatorActor(); err != nil || actor != "op:alice" {
		t.Fatalf("valid actor: %q %v", actor, err)
	}
}

func TestReviewApprovalExpiryAndReason(t *testing.T) {
	now := time.Now()
	for _, value := range []string{"", "0s", "-1m", "25h", "forever"} {
		if _, err := reviewApprovalExpiry(value, now); err == nil {
			t.Errorf("expiry %q accepted", value)
		}
	}
	if got, err := reviewApprovalExpiry("30m", now); err != nil || !got.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("valid expiry: %v %v", got, err)
	}
	if err := validateReviewReason(""); err == nil {
		t.Fatal("empty rejection/approval reason accepted")
	}
	if err := validateReviewReason("reviewed exact request"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewApproveSafeToolsNeedsAnOperatorActor(t *testing.T) {
	for _, actor := range []string{"", "chat", "agent:one", "op:"} {
		t.Setenv("FABRIC_ACTOR", actor)
		err := reviewApproveSafeTools([]string{"--grant", "/nonexistent", "--reason", "r"})
		if err == nil || !strings.Contains(err.Error(), "FABRIC_ACTOR=op:name") {
			t.Errorf("actor %q: %v", actor, err)
		}
	}
	t.Setenv("FABRIC_ACTOR", "op:alice")
	if err := reviewApproveSafeTools([]string{"--reason", "r"}); err == nil || !strings.Contains(err.Error(), "--grant") {
		t.Fatalf("no grant: %v", err)
	}
	if err := reviewApproveSafeTools([]string{"--grant", "/nonexistent", "--reason", ""}); err == nil {
		t.Fatal("no reason accepted")
	}
}

// goodqa#15864: the approval view shows the path, the file's sha256 and the
// change, and what a hostile file or path could do to the terminal is shown
// as text.
func TestReviewShowPrintsTheChangeAndNeutralisesControls(t *testing.T) {
	var out bytes.Buffer
	req := reviewgate.RequestSummary{RequestHash: strings.Repeat("a", 64), Tool: "edit",
		Target: &reviewgate.Binding{
			Path:         "dir/\x1b[2Jevil\u202e.go",
			TargetSHA256: strings.Repeat("b", 64), NewSHA256: strings.Repeat("c", 64), DiffSHA256: strings.Repeat("d", 64),
			Shown: "-old line\n+new line\x1b]0;owned\x07\n+rtl \u202e text\n", Truncated: true}}
	if err := printReviewDetail(&out, req); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{strings.Repeat("b", 64), strings.Repeat("d", 64), "-old line", "+new line", "--accept-truncated", "evil"} {
		if !strings.Contains(s, want) {
			t.Errorf("the view lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(s, bad) {
			t.Errorf("the view carries a raw control or bidi character %q", bad)
		}
	}
	if !strings.Contains(s, `\u{001B}`) || !strings.Contains(s, `\u{202E}`) {
		t.Errorf("controls are not shown as escapes:\n%s", s)
	}
}
