package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"fabric/internal/domain"
)

// A replay returns the first response byte for byte (Kimi P3.0 item 1). The
// control shows the jsonb column alone would not: its text differs.
func TestReplayBytesExact(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rb1")
	first := ok(t, st, ctx, "chat", "rb-admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rb1"})
	if len(first.Raw) == 0 || first.Replayed {
		t.Fatalf("first response: raw=%d replayed=%v", len(first.Raw), first.Replayed)
	}
	again := ok(t, st, ctx, "chat", "rb-admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rb1"})
	if !again.Replayed {
		t.Fatal("second call was not a replay")
	}
	if !bytes.Equal(again.Raw, first.Raw) {
		t.Fatalf("replay bytes differ:\nfirst  %s\nreplay %s", first.Raw, again.Raw)
	}
	var jsonbText string
	if err := st.pool.QueryRow(ctx, `SELECT response::text FROM commands WHERE actor = 'chat' AND command_id = 'rb-admit'`).Scan(&jsonbText); err != nil {
		t.Fatal(err)
	}
	if jsonbText == string(first.Raw) {
		t.Fatal("control is vacuous: the jsonb text equals the raw bytes, so this test proves nothing")
	}

	// Stored bytes cannot be rewritten (0011: commands are immutable); bytes
	// that disagree with the jsonb on insert are TestCommandRowChecks' case.
	if _, err := st.pool.Exec(ctx, `UPDATE commands SET response_bytes = convert_to('{"status":"ok","forged":1}', 'UTF8')
		WHERE actor = 'chat' AND command_id = 'rb-admit'`); err == nil {
		t.Fatal("stored response_bytes rewritten")
	}

	// A stored refusal replays byte for byte too, and still unwraps to its sentinel.
	_, err1 := st.Execute(ctx, "chat", "rb-bad", domain.Command{Kind: domain.CmdWake, Mission: "m_rb1"})
	_, err2 := st.Execute(ctx, "chat", "rb-bad", domain.Command{Kind: domain.CmdWake, Mission: "m_rb1"})
	var r1, r2 *Refusal
	if !errors.As(err1, &r1) || !errors.As(err2, &r2) || !r2.Response.Replayed {
		t.Fatalf("refusals: %v / %v", err1, err2)
	}
	if !bytes.Equal(r1.Response.Raw, r2.Response.Raw) || !errors.Is(err2, domain.ErrInvalidTransition) {
		t.Fatalf("refusal replay: %s vs %s (%v)", r1.Response.Raw, r2.Response.Raw, err2)
	}
}

// A row written before migration 0008 (no bytes, legacy format) replays from
// jsonb with the same meaning. Commands are immutable and new rows cannot
// claim the legacy format (0011), so the test writes the old row the way a
// restore would: as a copy of a real row with triggers off.
func TestReplayLegacyRowWithoutBytes(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_rb2")
	first := ok(t, st, ctx, "chat", "rb-legacy", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rb2"})
	if err := inTx(st,
		"SET LOCAL session_replication_role = replica",
		`CREATE TEMP TABLE legacy_row ON COMMIT DROP AS SELECT * FROM commands WHERE actor = 'chat' AND command_id = 'rb-legacy'`,
		`DELETE FROM commands WHERE actor = 'chat' AND command_id = 'rb-legacy'`,
		`UPDATE legacy_row SET response_bytes = NULL, canonical_request = NULL, request_format = 'LEGACY_HASH_ONLY'`,
		`INSERT INTO commands SELECT * FROM legacy_row`,
	); err != nil {
		t.Fatal(err)
	}
	var format string
	if err := st.pool.QueryRow(ctx, `SELECT request_format FROM commands WHERE actor = 'chat' AND command_id = 'rb-legacy'`).Scan(&format); err != nil || format != "LEGACY_HASH_ONLY" {
		t.Fatalf("setup: legacy row format %q, %v", format, err)
	}
	again := ok(t, st, ctx, "chat", "rb-legacy", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rb2"})
	if !again.Replayed || len(again.Raw) == 0 {
		t.Fatalf("legacy replay: %+v", again)
	}
	var a, b any
	if err := json.Unmarshal(first.Raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again.Raw, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("legacy replay changed meaning:\n%s\n%s", first.Raw, again.Raw)
	}
}
