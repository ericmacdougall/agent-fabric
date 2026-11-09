package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// AC-4 (spec v4.3): nothing written changes. The scripted flow below goes
// through the writer (Execute) only, then digests the named columns of every
// table it wrote. The goldens in testdata/writes-golden.json were generated
// by this same test run against the pre-change commit 207254da (write them
// with F1A_WRITES_GOLDEN=<path>); the new binary must reproduce every digest.
// Timestamps are database time and are dropped from the digests; ids that come
// from sequences are excluded.

var tsValue = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d`)

func digestRows(t *testing.T, st *Store, ctx context.Context, q string, args ...any) string {
	t.Helper()
	rows, err := st.pool.Query(ctx, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			if s, ok := v.(string); ok && tsValue.MatchString(s) {
				delete(m, k)
			}
		}
		b, _ := json.Marshal(m) // map keys marshal sorted
		lines = append(lines, string(b))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%d:%s", len(lines), hex.EncodeToString(sum[:]))
}

func TestWritesUnchanged(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_wu1")
	mk(t, st, ctx, "m_wu2")
	ok(t, st, ctx, "chat", "wu-a1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_wu1"})
	ok(t, st, ctx, "chat", "wu-a2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_wu2"})
	ok(t, st, ctx, "chat", "wu-sel", domain.Command{Kind: domain.CmdSelectCandidate, Mission: "m_wu1", Head: "sha_1"})
	fence := startAttempt(t, st, ctx, "m_wu1", "r_wu1")
	ok(t, st, ctx, "host", "wu-ack", domain.Command{Kind: domain.CmdAckStart, Mission: "m_wu1", Attempt: "r_wu1", Fence: fence})
	ok(t, st, ctx, "host", "wu-ev", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_wu1", Attempt: "r_wu1", Fence: fence, Revision: 0, Generation: 1, Head: "sha_1", Receipt: "rcpt_1"})
	eff := ok(t, st, ctx, "host", "wu-eff", domain.Command{Kind: domain.CmdSubmitEffect, Mission: "m_wu1", Attempt: "r_wu1", Fence: fence, EffectKind: "ticket_comment", Request: json.RawMessage(`{"ticket": 14791, "body": "checkpoint"}`)})
	eid := eff.Events[0].Effect
	ok(t, st, ctx, "fabd", "wu-unk", domain.Command{Kind: domain.CmdEffectUnknown, Mission: "m_wu1", Effect: eid, Submission: 1})
	ok(t, st, ctx, "fabd", "wu-res", domain.Command{Kind: domain.CmdResolveEffect, Mission: "m_wu1", Effect: eid, Submission: 1, Result: "CONFIRMED"})
	ok(t, st, ctx, "host", "wu-fin", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_wu1", Attempt: "r_wu1", Fence: fence, Outcome: "wait", WaitKey: "job_1"})
	ok(t, st, ctx, "host", "wu-wake", domain.Command{Kind: domain.CmdWake, Mission: "m_wu1", Token: "job_1"})
	ok(t, st, ctx, "fabd", "wu-rec", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_wu1", Attempt: "r_wu1", Fence: fence, Result: "VERIFIED"})
	f2 := startAttempt(t, st, ctx, "m_wu1", "r_wu2")
	ok(t, st, ctx, "host", "wu-ev2", domain.Command{Kind: domain.CmdRecordEvidence, Mission: "m_wu1", Attempt: "r_wu2", Fence: f2, Revision: 0, Generation: 1, Head: "sha_1", Receipt: "rcpt_2"})
	ok(t, st, ctx, "host", "wu-fin2", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_wu1", Attempt: "r_wu2", Fence: f2, Outcome: "ready_for_review", Claims: []domain.Claim{{AC: "AC-1", Receipt: "rcpt_2"}}})
	ok(t, st, ctx, "fabd", "wu-rec2", domain.Command{Kind: domain.CmdReconcileAttempt, Mission: "m_wu1", Attempt: "r_wu2", Fence: f2, Result: "VERIFIED"})
	ok(t, st, ctx, "chat", "wu-acc", domain.Command{Kind: domain.CmdAccept, Mission: "m_wu1", Attempt: "r_wu2"})
	ok(t, st, ctx, "fabd", "wu-del", domain.Command{Kind: domain.CmdDeliver, Mission: "m_wu1", Attempt: "r_wu2", Generation: 1})
	ok(t, st, ctx, "chat", "wu-rev", domain.Command{Kind: domain.CmdReviseIntent, Mission: "m_wu1", Reason: "user input"})
	// Refusals are part of the written record (the command row of a refusal).
	for i, c := range []domain.Command{
		{Kind: domain.CmdAdmitMission, Mission: "m_wu1"},
		{Kind: domain.CmdAckStart, Mission: "m_wu2", Attempt: "r_none", Fence: 7},
		{Kind: domain.CmdFinishAttempt, Mission: "m_wu1", Attempt: "r_wu1", Fence: 1, Outcome: "checkpoint"},
	} {
		if _, err := st.Execute(ctx, "host", domain.CommandID(fmt.Sprintf("wu-refuse-%d", i)), c); err == nil {
			t.Logf("command %d (%s) was accepted; its row is digested as written", i, c.Kind)
		}
	}

	got := map[string]string{
		"events":       digestRows(t, st, ctx, `SELECT jsonb_build_object('m', mission_id, 'seq', seq, 'kind', kind, 'actor', actor, 'cmd', command_id, 'payload', payload::text) FROM events WHERE mission_id LIKE 'm_wu%'`),
		"outbox":       digestRows(t, st, ctx, `SELECT jsonb_build_object('m', mission_id, 'seq', seq, 'kind', kind, 'payload', payload::text) FROM outbox WHERE mission_id LIKE 'm_wu%'`),
		"commands":     digestRows(t, st, ctx, `SELECT jsonb_build_object('cmd', command_id, 'req', encode(canonical_request, 'hex'), 'resp', encode(response_bytes, 'hex'), 'response', response::text) FROM commands WHERE command_id LIKE 'wu-%'`),
		"attempts":     digestRows(t, st, ctx, `SELECT to_jsonb(a) FROM attempts a WHERE mission_id LIKE 'm_wu%'`),
		"reservations": digestRows(t, st, ctx, `SELECT to_jsonb(r) FROM reservations r WHERE attempt_id LIKE 'r_wu%'`),
	}
	var schemaN int
	var schemaMax string
	if err := st.pool.QueryRow(ctx, `SELECT count(*), max(name) FROM fabric_schema`).Scan(&schemaN, &schemaMax); err != nil {
		t.Fatal(err)
	}
	got["schema"] = fmt.Sprintf("%d:%s", schemaN, schemaMax)

	if path := os.Getenv("F1A_WRITES_GOLDEN"); path != "" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	raw, err := os.ReadFile("testdata/writes-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s digest changed:\n got %s\nwant %s (pre-change commit 207254da)", k, got[k], w)
		}
	}
	if len(want) != len(got) {
		t.Errorf("golden has %d digests, run produced %d", len(want), len(got))
	}
}
