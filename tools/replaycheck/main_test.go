package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "replaycheck")) }

// The tool lists every mission with a digest, jsonb_eq true and status ok on
// a healthy store, and flags a mission whose log has a row it cannot read.
func TestRunListsEveryMission(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, id := range []domain.MissionID{"m_rc_a", "m_rc_b"} {
		if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#15372", "fabric", "main"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if err := run(ctx, st, &b, "test-commit"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"# build test-commit\n", "m_rc_a\t", "m_rc_b\t", "\ttrue\tok\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\tok\n") != 2 {
		t.Fatalf("want two ok rows:\n%s", out)
	}
	// An unreadable row is reported without its payload.
	if _, err := st.Pool().Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ('m_rc_b', 99, 'NEW_FUTURE_KIND', 'test', 'rc-x', '{"kind":"NEW_FUTURE_KIND","mission":"m_rc_b","seq":99,"secret":"CANARY"}')`); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := run(ctx, st, &b, "test-commit"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "m_rc_b\t-\t-\tunsupported\n") || strings.Contains(b.String(), "CANARY") {
		t.Fatalf("an unreadable mission was not flagged cleanly:\n%s", b.String())
	}
}
