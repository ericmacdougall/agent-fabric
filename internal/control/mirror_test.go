package control

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const mirrorIns = `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, recipient, body)
	VALUES ($1, $2, now(), $3, 'main', 'owner', $4)`

func mcTime(sec int) time.Time { return time.Date(2026, 11, 10, 1, 0, sec, 0, time.UTC) }

// goodqa#15586 AC-11: the chat mirror holds Mission Control's states, one row
// per id and state; changed content is skipped and counted; forged is sticky;
// the guard trigger sees the hash the generated column stores.
func TestMirrorStates(t *testing.T) {
	t.Run("two-states-and-replay", func(t *testing.T) {
		p, ctx := open(t)
		for i, st := range []string{"queued", "delivered"} {
			if _, err := p.Exec(ctx, mirrorIns, "m1", st, mcTime(i), "hello"); err != nil {
				t.Fatalf("state %s: %v", st, err)
			}
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.mc_chat_mirror WHERE id = 'm1'`); n != 2 {
			t.Fatalf("%d rows for one id in two states, want 2", n)
		}
		// A replay of the same id and state stores one row (the key is (id, state)).
		if tag, err := p.Exec(ctx, mirrorIns+` ON CONFLICT (id, state) DO NOTHING`, "m1", "queued", mcTime(0), "hello"); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("replay: %v rows %d, want a no-op", err, tag.RowsAffected())
		}
		if _, err := p.Exec(ctx, mirrorIns, "m1", "queued", mcTime(0), "hello"); code(err) != "23505" {
			t.Fatalf("plain replay: %v, want a unique violation", err)
		}
	})
	t.Run("changed-content", func(t *testing.T) {
		p, ctx := open(t)
		if _, err := p.Exec(ctx, mirrorIns, "m2", "queued", mcTime(0), "original"); err != nil {
			t.Fatal(err)
		}
		// Same id, same state, other body: skipped and counted (no unique violation, no row).
		if tag, err := p.Exec(ctx, mirrorIns, "m2", "queued", mcTime(1), "tampered"); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("changed content: %v rows %d, want skipped", err, tag.RowsAffected())
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.mirror_refusals WHERE id = 'm2'`); n != 1 {
			t.Fatalf("%d refusals recorded, want 1", n)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.mc_chat_mirror WHERE id = 'm2' AND body = 'tampered'`); n != 0 {
			t.Fatal("changed content was stored")
		}
	})
	t.Run("different-state-changed-content", func(t *testing.T) {
		p, ctx := open(t)
		if _, err := p.Exec(ctx, mirrorIns, "m3", "queued", mcTime(0), "original"); err != nil {
			t.Fatal(err)
		}
		if tag, err := p.Exec(ctx, mirrorIns, "m3", "answered", mcTime(1), "other body"); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("other state, other content: %v rows %d, want skipped", err, tag.RowsAffected())
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.mirror_refusals WHERE id = 'm3' AND state = 'answered'`); n != 1 {
			t.Fatalf("%d refusals, want 1", n)
		}
	})
	t.Run("forged-sticky-and-latest", func(t *testing.T) {
		p, ctx := open(t)
		for _, c := range []struct {
			id, st string
			sec    int
		}{{"f1", "queued", 0}, {"f1", "forged", 1}, {"f1", "answered", 2}, {"f2", "queued", 0}, {"f2", "delivered", 1}, {"f2", "queued", 5}} {
			if _, err := p.Exec(ctx, mirrorIns+` ON CONFLICT (id, state) DO NOTHING`, c.id, c.st, mcTime(c.sec), "same"); err != nil {
				t.Fatal(err)
			}
		}
		var st string
		if err := p.QueryRow(ctx, `SELECT state FROM control.mc_chat_latest WHERE id = 'f1'`).Scan(&st); err != nil || st != "forged" {
			t.Fatalf("f1 latest %q %v, want forged (sticky over a later answered)", st, err)
		}
		// Stickiness must not depend on forged also having the top rank: give
		// answered a higher rank than forged and forged must still win.
		if _, err := p.Exec(ctx, `UPDATE control.mirror_states SET rank = 200 WHERE state = 'answered'`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Exec(ctx, `UPDATE control.mirror_states SET rank = 5 WHERE state = 'answered'`) }) //nolint:errcheck
		if err := p.QueryRow(ctx, `SELECT state FROM control.mc_chat_latest WHERE id = 'f1'`).Scan(&st); err != nil || st != "forged" {
			t.Fatalf("f1 latest %q %v with answered ranked above forged, want forged (sticky flag)", st, err)
		}
		if err := p.QueryRow(ctx, `SELECT state FROM control.mc_chat_latest WHERE id = 'f2'`).Scan(&st); err != nil || st != "delivered" {
			t.Fatalf("f2 latest %q %v, want delivered (highest rank, not the latest mc_ts)", st, err)
		}
	})
	t.Run("supplied-hash", func(t *testing.T) {
		p, ctx := open(t)
		_, err := p.Exec(ctx, `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body, content_sha256)
			VALUES ('h1', 'queued', now(), now(), 'main', 'b', repeat('0', 64))`)
		if code(err) != "428C9" {
			t.Fatalf("supplied content_sha256: %v, want SQLSTATE 428C9 (generated column)", err)
		}
	})
	t.Run("trigger-hash-equals-stored", func(t *testing.T) {
		p, ctx := open(t)
		if _, err := p.Exec(ctx, mirrorIns, "t1", "queued", mcTime(0), "payload"); err != nil {
			t.Fatal(err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.mc_chat_mirror m JOIN control.mirror_content c ON c.id = m.id
			WHERE m.id = 't1' AND m.content_sha256 = c.content_sha256 AND m.content_sha256 = control.mirror_hash(m.sender, m.recipient, m.body)`); n != 1 {
			t.Fatalf("trigger hash and stored hash differ (%d matching rows, want 1)", n)
		}
	})
	t.Run("null-parts", func(t *testing.T) {
		p, ctx := open(t)
		if _, err := p.Exec(ctx, `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, recipient, body) VALUES ('n1', 'queued', now(), now(), 'main', NULL, 'b')`); code(err) != "23502" {
			t.Fatalf("NULL recipient: %v, want a not-null violation", err)
		}
	})
	t.Run("concurrent-sessions", func(t *testing.T) {
		p, ctx := open(t)
		for round := 0; round < 50; round++ {
			id := fmt.Sprintf("c%d", round)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i, st := range []string{"queued", "delivered"} {
				wg.Add(1)
				go func(i int, st string) {
					defer wg.Done()
					<-start
					p.Exec(ctx, mirrorIns, id, st, mcTime(i), fmt.Sprintf("body %d", i)) //nolint:errcheck // one of the two is skipped by design
				}(i, st)
			}
			close(start)
			wg.Wait()
			if n := count(t, p, ctx, `SELECT count(DISTINCT content_sha256) FROM control.mc_chat_mirror WHERE id = $1`, id); n != 1 {
				t.Fatalf("round %d: %d distinct contents stored for one id, want exactly 1", round, n)
			}
			if n := count(t, p, ctx, `SELECT count(*) FROM control.mirror_refusals WHERE id = $1`, id); n != 1 {
				t.Fatalf("round %d: %d refusals, want 1", round, n)
			}
		}
	})
}

// goodqa#15586 AC-12: numbered schema files, applied in order, recorded with
// their hashes; changed hashes, gaps and unknown applied versions are refused.
func TestBootstrapUpgrade(t *testing.T) {
	fresh := func(t *testing.T) (*pgxpool.Pool, context.Context) {
		t.Helper()
		p, ctx := open(t)
		if _, err := p.Exec(ctx, "DROP SCHEMA control CASCADE"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { // put the real schema back for the tests that follow
			p.Exec(ctx, "DROP SCHEMA IF EXISTS control CASCADE") //nolint:errcheck
			if err := Bootstrap(ctx, p); err != nil {
				t.Errorf("restoring the control schema: %v", err)
			}
		})
		return p, ctx
	}
	v1 := "CREATE TABLE control.t1 (a int);\n"
	v2 := "CREATE TABLE control.t2 (b int);\n"
	build := func(files map[string]string) []step {
		m := fstest.MapFS{}
		for n, c := range files {
			m["sql/"+n] = &fstest.MapFile{Data: []byte(c)}
		}
		st, err := loadSteps(m, "sql")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	t.Run("applies-later-versions", func(t *testing.T) {
		p, ctx := fresh(t)
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1})); err != nil {
			t.Fatal(err)
		}
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1, "002_b.sql": v2})); err != nil {
			t.Fatalf("upgrade 1 to 2: %v", err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.bootstrap_ledger`); n != 2 {
			t.Fatalf("%d ledger rows, want 2", n)
		}
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1, "002_b.sql": v2})); err != nil {
			t.Fatalf("rerun: %v", err)
		}
	})
	t.Run("changed-hash", func(t *testing.T) {
		p, ctx := fresh(t)
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1})); err != nil {
			t.Fatal(err)
		}
		err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1 + "-- edited\n", "002_b.sql": v2}))
		if err == nil || !strings.Contains(err.Error(), ErrDefinitionChanged.Error()) {
			t.Fatalf("changed applied version: %v, want ErrDefinitionChanged", err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'control' AND table_name = 't2'`); n != 0 {
			t.Fatal("a later version was applied despite the refusal")
		}
	})
	t.Run("gap", func(t *testing.T) {
		if _, err := loadSteps(fstest.MapFS{"sql/001_a.sql": {Data: []byte(v1)}, "sql/003_c.sql": {Data: []byte(v2)}}, "sql"); err == nil || !strings.Contains(err.Error(), "gap") {
			t.Fatalf("files 001 and 003: %v, want a gap error", err)
		}
		p, ctx := fresh(t)
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1, "002_b.sql": v2})); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `DELETE FROM control.bootstrap_ledger WHERE version = 1`); err != nil {
			t.Fatal(err)
		}
		err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1, "002_b.sql": v2}))
		if err == nil || !strings.Contains(err.Error(), ErrDefinitionChanged.Error()) {
			t.Fatalf("version 2 applied without version 1: %v, want a refusal", err)
		}
	})
	t.Run("old-fixture", func(t *testing.T) {
		p, ctx := fresh(t)
		if _, err := p.Exec(ctx, `CREATE SCHEMA control; CREATE TABLE control.bootstrap_ledger (version integer PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());
			INSERT INTO control.bootstrap_ledger (version, sha256) VALUES (1, 'a-hash-of-the-earlier-definition')`); err != nil {
			t.Fatal(err)
		}
		err := Bootstrap(ctx, p)
		if err == nil || !strings.Contains(err.Error(), ErrDefinitionChanged.Error()) {
			t.Fatalf("a database from the earlier definition: %v, want a refusal", err)
		}
	})
	t.Run("unknown-applied-version", func(t *testing.T) {
		p, ctx := fresh(t)
		if err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1, "002_b.sql": v2})); err != nil {
			t.Fatal(err)
		}
		err := bootstrapSteps(ctx, p, build(map[string]string{"001_a.sql": v1}))
		if err == nil || !strings.Contains(err.Error(), ErrDefinitionChanged.Error()) {
			t.Fatalf("an applied version newer than the binary: %v, want a refusal", err)
		}
	})
}
