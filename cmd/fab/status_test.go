package main

import (
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// goodqa#15087 AC-3: `fab status` shows each mission's provider, and an
// admitted anthropic-api mission with no attempt as awaiting
// `mission run --grant`.
func TestStatusShowsProvider(t *testing.T) {
	rows := []store.MissionRow{
		{ID: "m_api", Provider: domain.ProviderAnthropicAPI, Priority: 1, Execution: domain.ExecAdmitted, Candidate: "NONE", Delivery: "NOT_READY",
			Ticket: "GoodQA goodqa#1", Name: "native", AwaitingGrant: true},
		{ID: "m_cc", Provider: domain.ProviderClaudeCode, Priority: 2, Execution: domain.ExecRunning, Candidate: "NONE", Delivery: "NOT_READY",
			Version: domain.Version{AttemptFence: 3}, Ticket: "GoodQA goodqa#2", Name: "fabd"},
	}
	var b strings.Builder
	if err := writeMissions(&b, rows); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"MISSION  PROVIDER       PRIO  EXEC                                     CAND  DELIV      REV/FENCE/GEN  TICKET           NAME\n" +
		"m_api    anthropic-api  1     ADMITTED (awaiting mission run --grant)  NONE  NOT_READY  0/0/0          GoodQA goodqa#1  native\n" +
		"m_cc     claude-code    2     RUNNING                                  NONE  NOT_READY  0/3/0          GoodQA goodqa#2  fabd\n"
	if b.String() != want {
		t.Fatalf("status table:\n%s\nwant:\n%s", b.String(), want)
	}
}
