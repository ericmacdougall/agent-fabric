package native_test

import (
	"strings"
	"testing"

	"fabric/internal/native"
)

// capGrant is a grant that passes every check except the caps under test.
func capGrant(month, job, run int64) native.Grant {
	g := native.Grant{Provider: "anthropic", Mission: "m_cap1", Attempt: "r_cap1", Ticket: "GoodQA goodqa#15086",
		Model: "test-model", MaxTokens: 100, Prompt: "p", Worktree: "/w", RunDir: "/r",
		BaseURL: "https://api.anthropic.com", AnthropicVersion: "2023-06-01", KeyCommand: []string{"true"},
		MaxTurns: 1, WallSeconds: 1}
	g.Caps.Month, g.Caps.Job, g.Caps.Run = month, job, run
	return g
}

// goodqa#15086: the fabric_native month ceiling is USD 20, fixed in code.
func TestGrantMonthCeiling(t *testing.T) {
	if err := capGrant(20_000_000, 5_000_000, 1_000_000).Validate(); err != nil {
		t.Fatalf("a grant at the USD 20 ceiling was refused: %v", err)
	}
	for _, month := range []int64{20_000_001, 25_000_000, 100_000_000} {
		err := capGrant(month, 5_000_000, 1_000_000).Validate()
		if err == nil {
			t.Errorf("month cap %d micros accepted above the USD 20 ceiling", month)
			continue
		}
		if !strings.Contains(err.Error(), "month 20 USD") {
			t.Errorf("refusal does not name the ceiling: %v", err)
		}
	}
	if native.MonthCapMicros != 20_000_000 {
		t.Errorf("exported month ceiling is %d micros, want 20_000_000 (metrics read it)", native.MonthCapMicros)
	}
}
