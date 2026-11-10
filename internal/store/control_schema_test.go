package store

import (
	"strings"
	"testing"
)

// goodqa#15586 F19a AC-1/AC-11 (storage): migration 0039 adds the `control`
// schema forward from level 38 without touching any public table, applies once,
// and re-applies cleanly.
func TestMigrate0039FromLevel38(t *testing.T) {
	st, ctx, _ := freshStore(t, migration0038)
	n38, latest := level(t, st, ctx)
	if latest != migration0038 {
		t.Fatalf("setup: level %d at %s, want %s", n38, latest, migration0038)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'control'`); n != 0 {
		t.Fatal("setup: level 38 already has the control schema")
	}
	publicBefore := count(t, st, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate 38 -> 39: %v", err)
	}
	if n, latest := level(t, st, ctx); n != n38+1 || latest != migration0039 {
		t.Fatalf("level %d at %s, want %d at %s", n, latest, n38+1, migration0039)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`); n != publicBefore {
		t.Fatalf("%d public tables after 0039, want %d: the migration must be additive and stay in control", n, publicBefore)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'control'`); n != 5 {
		t.Fatalf("%d control tables, want 5 (2 source, 3 projection)", n)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := st.pool.Exec(ctx, mustRead(t, migration0039)); err != nil {
		t.Fatalf("re-applying 0039: %v", err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'control' AND NOT t.tgisinternal`); n != 4 {
		t.Fatalf("%d triggers on control tables after re-apply, want 4 (update/delete and truncate on each source table)", n)
	}
	if !strings.Contains(mustRead(t, migration0039), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("0039 does not set its lock_timeout")
	}
}

const ctlIngestInsert = `INSERT INTO control.ingested_events (id, ts, kind, source, summary) VALUES ($1, now(), 'wal', 'harness', $2)`

// The source tables are append-only: no UPDATE, DELETE or TRUNCATE, a replay of
// an id stores one row.
func TestControlSourceTablesAppendOnly(t *testing.T) {
	st, ctx := openTest(t)
	if _, err := st.pool.Exec(ctx, ctlIngestInsert, "harness:e1", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO control.mc_chat_mirror (id, ts, sender, body) VALUES ('mc:c1', now(), 'main', 'hello')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE control.ingested_events SET summary = 'x' WHERE id = 'harness:e1'`,
		`DELETE FROM control.ingested_events WHERE id = 'harness:e1'`,
		`TRUNCATE control.ingested_events`,
		`UPDATE control.mc_chat_mirror SET body = 'x' WHERE id = 'mc:c1'`,
		`DELETE FROM control.mc_chat_mirror WHERE id = 'mc:c1'`,
		`TRUNCATE control.mc_chat_mirror`,
	} {
		_, err := st.pool.Exec(ctx, q)
		if pgCode(err) != "23001" || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want SQLSTATE 23001 (append-only)", q, err)
		}
	}
	// Control: a replay of the same id is refused by the primary key, so it stores one row.
	if _, err := st.pool.Exec(ctx, ctlIngestInsert, "harness:e1", "again"); pgCode(err) != "23505" {
		t.Errorf("replayed id: %v, want a unique violation", err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM control.ingested_events`); n != 1 {
		t.Fatalf("%d rows after the refused mutations and the replay, want 1", n)
	}
}

// Value limits that keep a hostile or oversized producer out of the page.
func TestControlConstraints(t *testing.T) {
	st, ctx := openTest(t)
	long := strings.Repeat("a", 281)
	ok := strings.Repeat("a", 280)
	for _, c := range []struct{ what, id, summary string }{
		{"id without a source prefix", "nocolon", "s"},
		{"id from an upper-case source", "Harness:e1", "s"},
		{"id with a control character", "harness:e\x1b1", "s"},
		{"empty id suffix", "harness:", "s"},
		{"summary of 281 characters", "harness:long", long},
		{"summary with a terminal escape", "harness:esc", "a\x1b[31mred"},
		{"summary with a newline", "harness:nl", "a\nb"},
	} {
		if _, err := st.pool.Exec(ctx, ctlIngestInsert, c.id, c.summary); pgCode(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", c.what, err)
		}
	}
	if _, err := st.pool.Exec(ctx, ctlIngestInsert, "harness:fits", ok); err != nil {
		t.Errorf("a summary of exactly 280 characters: %v, want accepted", err)
	}
	for _, q := range []string{
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary) VALUES ('harness:k', now(), 'bogus', 'harness', 's')`,
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary, count) VALUES ('harness:c0', now(), 'wal', 'harness', 's', 0)`,
		`INSERT INTO control.mc_chat_mirror (id, origin, ts, sender, body) VALUES ('c:fab', 'fabric', now(), 'main', 'b')`,
		`INSERT INTO control.mc_chat_mirror (id, ts, sender, body) VALUES ('c:big', now(), 'main', repeat('b', 4001))`,
		`INSERT INTO control.initiative_state (initiative, state, as_of) VALUES ('i1', 'exploding', now())`,
		`INSERT INTO control.proj_cursor (consumer, last_id) VALUES ('control.proj', -1)`,
	} {
		if _, err := st.pool.Exec(ctx, q); pgCode(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", q, err)
		}
	}
}

// Nothing in control is granted to PUBLIC, and Store.Truncate (the tests'
// reset) clears every control table.
func TestControlNoPublicGrantsAndReset(t *testing.T) {
	st, ctx := openTest(t)
	if n := count(t, st, ctx, `SELECT count(*) FROM information_schema.role_table_grants WHERE table_schema = 'control' AND grantee = 'PUBLIC'`); n != 0 {
		t.Fatalf("%d PUBLIC table grants in control, want 0", n)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname = 'control' AND a.grantee = 0`); n != 0 {
		t.Fatalf("%d PUBLIC schema grants on control, want 0", n)
	}
	if _, err := st.pool.Exec(ctx, ctlIngestInsert, "harness:r1", "s"); err != nil {
		t.Fatal(err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM control.ingested_events`); n != 0 {
		t.Fatalf("%d ingested events after the test reset, want 0", n)
	}
}
