package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15966 AC-19, NEG-15: a refused token fails the attempt definite on
// its first try, the reason is fixed text without the CLI's message, and
// the mission is held so no retry runs with the refused token.
func TestRefusedTokenFailsDefiniteAndHolds(t *testing.T) {
	const cliText = "OAuth token has expired SECRET-LOOKING-CLI-TEXT"
	for name, lines := range map[string][]string{
		"assistant error then result": {initEv,
			`{"type":"assistant","session_id":"s1","error":"authentication_failed","message":{"usage":{}}}`,
			`{"type":"result","subtype":"success","session_id":"s1","uuid":"r1","num_turns":1,"is_error":true,"result":"` + cliText + `"}`},
		"401 retry without a result": {initEv,
			`{"type":"system","subtype":"api_retry","session_id":"s1","error":"` + cliText + `","error_status":401}`},
	} {
		t.Run(name, func(t *testing.T) {
			id := strings.ReplaceAll(name, " ", "_")
			m := domain.MissionID("m_" + id)
			a := domain.AttemptID("r_" + id + "_1")
			st, ln, cfg := ownerSetup(t, m)
			ctx := context.Background()
			resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: a, Host: cfg.Host, Role: "writer"})
			if err != nil {
				t.Fatal(err)
			}
			fence := resp.Events[0].Fence
			if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := New(st, cfg, ln)
			if err := s.afterWait(ctx, m, a, fence, dir, 1, nil); err != nil {
				t.Fatal(err)
			}
			var state, reason string
			st.Pool().QueryRow(ctx, "SELECT state FROM attempts WHERE id = $1", string(a)).Scan(&state) //nolint:errcheck
			if state != string(domain.AttemptFailedDefinite) {
				t.Fatalf("state %s, want FAILED_DEFINITE", state)
			}
			st.Pool().QueryRow(ctx, "SELECT string_agg(payload::text, ' ') FROM events WHERE command_id = $1", "finish:"+string(a)).Scan(&reason) //nolint:errcheck
			if !strings.Contains(reason, "credential_rejected") || strings.Contains(reason, "SECRET-LOOKING") {
				t.Fatalf("finish reason not the fixed credential_rejected text")
			}
			held, err := st.Held(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, h := range held {
				if h.Mission == m {
					found = true
				}
			}
			if !found {
				t.Fatal("mission not held after a refused token")
			}
		})
	}
}
