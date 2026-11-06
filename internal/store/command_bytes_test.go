package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

// 0011 (Kimi P3.0): a new command row carries its canonical request bytes,
// the database checks the stored hash against them, and the format column
// says which rows predate this (legacy, hash only).
func TestCommandRequestBytesStored(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_cb1")
	cmd := domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cb1"}
	ok(t, st, ctx, "chat", "cb-admit", cmd)
	var format, hash string
	var canon []byte
	if err := st.pool.QueryRow(ctx, `SELECT request_format, request_sha256, canonical_request FROM commands WHERE command_id = 'cb-admit'`).
		Scan(&format, &hash, &canon); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	if format != "CANONICAL_V1" || len(canon) == 0 || hex.EncodeToString(sum[:]) != hash || hash != cmd.RequestHash() {
		t.Fatalf("format %q, %d request bytes, hash %s (bytes hash %x, command hash %s)", format, len(canon), hash, sum, cmd.RequestHash())
	}
	if string(canon) != string(cmd.CanonicalRequest()) {
		t.Fatalf("stored request bytes differ from the canonical form:\n%s\n%s", canon, cmd.CanonicalRequest())
	}
}

// Rows the code must never write are refused by the database.
func TestCommandRowChecks(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_cb2")
	ok(t, st, ctx, "chat", "cb2-admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_cb2"})
	codes := map[string]bool{"23514": true, "23001": true}
	for _, q := range []string{
		// a canonical row without request bytes
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes)
		 VALUES ('chat', 'm_cb2', 'x1', 'WAKE', 'ab', 'ok', '{}', '{}')`,
		// a canonical row whose hash does not match its bytes
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes, canonical_request)
		 VALUES ('chat', 'm_cb2', 'x2', 'WAKE', 'ab', 'ok', '{}', '{}', '{"kind":"WAKE"}')`,
		// a canonical row without response bytes
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, canonical_request)
		 VALUES ('chat', 'm_cb2', 'x3', 'WAKE', encode(sha256('{}'::bytea), 'hex'), 'ok', '{}', '{}')`,
		// history is immutable
		`UPDATE commands SET status = 'refused' WHERE command_id = 'cb2-admit'`,
		`DELETE FROM commands WHERE command_id = 'cb2-admit'`,
		`UPDATE events SET kind = 'MISSION_RELEASED' WHERE mission_id = 'm_cb2'`,
		`DELETE FROM events WHERE mission_id = 'm_cb2'`,
		// only known formats, and a new row may not claim the legacy format
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes, canonical_request, request_format)
		 VALUES ('chat', 'm_cb2', 'x4', 'WAKE', encode(sha256('{}'::bytea), 'hex'), 'ok', '{}', '{}', '{}', 'CANONICAL_V2')`,
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, request_format)
		 VALUES ('chat', 'm_cb2', 'x5', 'WAKE', 'ab', 'ok', '{}', 'LEGACY_HASH_ONLY')`,
		// response bytes that disagree with the jsonb
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes, canonical_request)
		 VALUES ('chat', 'm_cb2', 'x6', 'WAKE', encode(sha256('{}'::bytea), 'hex'), 'ok', '{}', '{"forged":1}', '{}')`,
		// no TRUNCATE of history
		`TRUNCATE commands`,
		`TRUNCATE events CASCADE`,
	} {
		_, err := st.pool.Exec(ctx, q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !codes[pgErr.Code] {
			t.Errorf("not refused by a constraint (%v): %s", err, q)
		}
	}
}

// With triggers off (a restore), the CHECKs alone still refuse an unknown
// format and a legacy row that claims request bytes.
func TestCommandChecksHoldWithTriggersOff(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_cb3")
	for _, q := range []string{
		// Shaped like a legacy row, so only the format-known CHECK refuses it.
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, request_format)
		 VALUES ('chat', 'm_cb3', 'y1', 'WAKE', encode(sha256('{}'::bytea), 'hex'), 'ok', '{}', 'CANONICAL_V2')`,
		`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, canonical_request, request_format)
		 VALUES ('chat', 'm_cb3', 'y2', 'WAKE', encode(sha256('{}'::bytea), 'hex'), 'ok', '{}', '{}', 'LEGACY_HASH_ONLY')`,
	} {
		err := inTx(st, "SET LOCAL session_replication_role = replica", q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("with triggers off, want 23514 (%v): %s", err, q)
		}
	}
}
