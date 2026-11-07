package native_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// preSpend charges micros to a spend resource through the real ledger, as an
// earlier native run in that month would have: a live attempt holds, then
// settles the actual cost.
func preSpend(t *testing.T, st *store.Store, resource string, micros int64) {
	t.Helper()
	ctx := context.Background()
	m, a := domain.MissionID("m_premonth"), domain.AttemptID("r_premonth")
	if err := st.CreateMissionSpec(ctx, store.MissionSpec{Provider: domain.ProviderClaudeCode, ID: m, Name: "earlier run", Ticket: "GoodQA goodqa#15086", Priority: 100, Source: "ticket:GoodQA goodqa#15086"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "native-runner", "pre-admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	resp, err := st.Execute(ctx, "native-runner", "pre-start", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: "h_local", Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "native-runner", "pre-ack", domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	led := &agent.Ledger{St: st, Attempt: a, Fence: fence, TTL: time.Hour, Caps: []agent.Cap{{Resource: resource, Total: native.MonthCapMicros}}}
	rs, err := led.Hold(ctx, "pre-hold", micros)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Settle(ctx, rs, micros); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "native-runner", "pre-finish", domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: a, Fence: fence, Outcome: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
}

// goodqa#15086 AC-2: with month spend already near the USD 20 ceiling, the
// loop sends no request whose worst case would cross it. The same spend
// recorded against another month does not block the run (the control).
func TestMonthCeilingStopsLoop(t *testing.T) {
	ctx := context.Background()
	thisMonth := func() string { return time.Now().UTC().Format("200601") }
	for _, tc := range []struct {
		name      string
		thisMonth bool // spend charged to the month Run will use; otherwise to 199901
		wantSent  bool
		wantKind  string
	}{
		{"this month near the ceiling", true, false, agent.OutcomeBudgetExhausted},
		{"same spend in another month", false, true, agent.OutcomeDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run picks the UTC month itself; if the month turns over during
			// the case, the seed and the run disagree, so the case runs again
			// once (it cannot turn over twice in a few seconds).
			for try := 0; try < 2; try++ {
				month := thisMonth()
				spentIn := "199901"
				if tc.thisMonth {
					spentIn = month
				}
				st := storetest.Open(t)
				preSpend(t, st, agent.SpendResource("month", spentIn), 19_999_950)
				e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("finished", 50, 5)})
				r, err := native.Run(ctx, st, e.g, opts(true))
				if thisMonth() != month && try == 0 {
					continue
				}
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				sent := len(e.srv.Requests()) > 0
				if sent != tc.wantSent || r.Outcome != tc.wantKind {
					t.Fatalf("outcome %q, requests %d; want outcome %q, sent=%v", r.Outcome, len(e.srv.Requests()), tc.wantKind, tc.wantSent)
				}
				if res := agent.SpendResource("month", month); !tc.wantSent && !strings.Contains(r.Note, res) {
					t.Fatalf("budget note %q does not name the month resource %s", r.Note, res)
				}
				return
			}
		})
	}
}
