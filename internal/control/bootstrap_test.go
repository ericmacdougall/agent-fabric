package control

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "ctl")) }

// open returns the test store's pool with the control schema bootstrapped and
// emptied (Store.Truncate clears it when it exists).
func open(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	if err := Bootstrap(ctx, st.Pool()); err != nil {
		t.Fatal(err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatal(err)
	}
	return st.Pool(), ctx
}

func count(t *testing.T, p *pgxpool.Pool, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func code(err error) string {
	if pe, ok := err.(*pgconn.PgError); ok {
		return pe.Code
	}
	return ""
}

// AC-13: bootstrap creates the schema without touching fabric_schema or any
// public table, and a second call is a no-op.
func TestBootstrapLeavesFabricAlone(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	p := st.Pool()
	if _, err := p.Exec(ctx, "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	ledger := func() string {
		var s string
		if err := p.QueryRow(ctx, `SELECT count(*) || ':' || max(name) || ':' || string_agg(sha256, ',' ORDER BY name) FROM fabric_schema`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	publicTables := func() int {
		return count(t, p, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`)
	}
	beforeLedger, beforeTables := ledger(), publicTables()
	if err := Bootstrap(ctx, p); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Bootstrap(ctx, p); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if got := ledger(); got != beforeLedger {
		t.Fatalf("fabric_schema changed: %s -> %s", beforeLedger, got)
	}
	if n := publicTables(); n != beforeTables {
		t.Fatalf("%d public tables after bootstrap, want %d", n, beforeTables)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'control' AND table_name <> 'bootstrap_ledger'`); n != 10 {
		t.Fatalf("%d control tables, want 10 (4 source tables, 3 projection tables, 2 reference tables, 1 view)", n)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM control.bootstrap_ledger`); n != 1 {
		t.Fatalf("%d ledger rows, want 1", n)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'control' AND NOT t.tgisinternal`); n != 10 {
		t.Fatalf("%d triggers, want 10", n)
	}
}

// A schema made from a different definition is refused, not trusted.
func TestBootstrapRefusesChangedDefinition(t *testing.T) {
	p, ctx := open(t)
	if _, err := p.Exec(ctx, `UPDATE control.bootstrap_ledger SET sha256 = 'deadbeef'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // the ledger is not reset by Truncate; leave it as found
		p.Exec(ctx, `UPDATE control.bootstrap_ledger SET sha256 = $1`, SchemaSHA256()) //nolint:errcheck
	})
	err := Bootstrap(ctx, p)
	if err == nil || !strings.Contains(err.Error(), ErrDefinitionChanged.Error()) {
		t.Fatalf("bootstrap over a changed definition: %v, want ErrDefinitionChanged", err)
	}
}

const ins = `INSERT INTO control.ingested_events (id, ts, kind, source, summary) VALUES ($1, now(), 'wal', $2, $3)`

// The source tables are append-only for every role, including one with the
// TRUNCATE privilege that sets the public tables' test exemption.
func TestSourceTablesAppendOnly(t *testing.T) {
	p, ctx := open(t)
	if _, err := p.Exec(ctx, ins, "harness:e1", "harness", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('mc:c1', 'queued', now(), now(), 'main', 'hello')`); err != nil {
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
		_, err := p.Exec(ctx, q)
		if code(err) != "23001" || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want SQLSTATE 23001 (append-only)", q, err)
		}
	}
	// The public tables' truncate exemption must not apply here.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL fabric.allow_truncate = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE control.ingested_events`); code(err) != "23001" {
		t.Errorf("TRUNCATE with fabric.allow_truncate on: %v, want refused", err)
	}
}

// A replay of the same id is one row; a producer cannot choose seq.
func TestReplayAndSeq(t *testing.T) {
	p, ctx := open(t)
	if _, err := p.Exec(ctx, ins, "harness:e1", "harness", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, ins, "harness:e1", "harness", "again"); code(err) != "23505" {
		t.Errorf("replayed id: %v, want a unique violation", err)
	}
	if tag, err := p.Exec(ctx, ins+` ON CONFLICT (id) DO NOTHING`, "harness:e1", "harness", "again"); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("ON CONFLICT (id) DO NOTHING: %v rows %d, want a no-op", err, tag.RowsAffected())
	}
	if _, err := p.Exec(ctx, `INSERT INTO control.ingested_events (seq, id, ts, kind, source, summary) VALUES (1, 'harness:e2', now(), 'wal', 'harness', 's')`); code(err) != "428C9" {
		t.Errorf("explicit seq: %v, want SQLSTATE 428C9 (generated always)", err)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM control.ingested_events`); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// Value limits that keep a hostile or oversized producer out of the page, and
// the Fabric-origin backstop.
func TestConstraints(t *testing.T) {
	p, ctx := open(t)
	long, fits := strings.Repeat("a", 281), strings.Repeat("a", 280)
	for _, c := range []struct{ what, id, source, summary string }{
		{"id without a source prefix", "nocolon", "harness", "s"},
		{"id from an upper-case source", "Harness:e1", "Harness", "s"},
		{"id with a control character", "harness:e\x1b1", "harness", "s"},
		{"empty id suffix", "harness:", "harness", "s"},
		{"source that is not the id prefix", "harness:x", "other", "s"},
		{"Fabric origin by id and source", "fabric:42", "fabric", "s"},
		{"summary of 281 characters", "harness:long", "harness", long},
		{"summary with a terminal escape", "harness:esc", "harness", "a\x1b[31mred"},
		{"summary with a newline", "harness:nl", "harness", "a\nb"},
	} {
		if _, err := p.Exec(ctx, ins, c.id, c.source, c.summary); code(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", c.what, err)
		}
	}
	if _, err := p.Exec(ctx, ins, "harness:fits", "harness", fits); err != nil {
		t.Errorf("a summary of exactly 280 characters: %v, want accepted", err)
	}
	for _, q := range []string{
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary) VALUES ('harness:k', now(), 'bogus', 'harness', 's')`,
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary, count) VALUES ('harness:c0', now(), 'wal', 'harness', 's', 0)`,
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary, actor) VALUES ('harness:a', now(), 'wal', 'harness', 's', E'x\x1b]8;;http://e\x07y')`,
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary, initiative) VALUES ('harness:i', now(), 'wal', 'harness', 's', E'a\nb')`,
		`INSERT INTO control.ingested_events (id, ts, kind, source, summary, tenant) VALUES ('harness:t', now(), 'wal', 'harness', 's', E'a\x01')`,
		`INSERT INTO control.mc_chat_mirror (id, state, origin, ts, mc_ts, sender, body) VALUES ('c:fab', 'queued', 'fabric', now(), now(), 'main', 'b')`,
		`INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('c:big', 'queued', now(), now(), 'main', repeat('b', 4001))`,
		`INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('c:esc', 'queued', now(), now(), 'main', E'a\x1b[2Jb')`,
		`INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('c:snd', 'queued', now(), now(), E'ma\nin', 'b')`,
		`INSERT INTO control.initiative_state (initiative, state, as_of) VALUES ('i1', 'exploding', now())`,
		`INSERT INTO control.initiative_state (initiative, state, as_of) VALUES (E'i\x1b1', 'idle', now())`,
		`INSERT INTO control.proj_cursor (consumer, last_id) VALUES ('control.proj', -1)`,
	} {
		if _, err := p.Exec(ctx, q); code(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", q, err)
		}
	}
	// Controls: a line feed is allowed in a chat body, nothing else is.
	if _, err := p.Exec(ctx, `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('c:nl', 'queued', now(), now(), 'main', E'line one\nline two')`); err != nil {
		t.Errorf("chat body with a line feed: %v, want accepted", err)
	}
}

// Nothing in control is granted to PUBLIC, and Store.Truncate (the tests'
// reset) clears every control table including the append-only ones.
func TestNoPublicGrantsAndReset(t *testing.T) {
	p, ctx := open(t)
	if n := count(t, p, ctx, `SELECT count(*) FROM information_schema.role_table_grants WHERE table_schema = 'control' AND grantee = 'PUBLIC'`); n != 0 {
		t.Fatalf("%d PUBLIC table grants in control, want 0", n)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname = 'control' AND a.grantee = 0`); n != 0 {
		t.Fatalf("%d PUBLIC schema grants on control, want 0", n)
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace, aclexplode(f.proacl) a
		WHERE n.nspname = 'control' AND a.grantee = 0`); n != 0 {
		t.Fatalf("%d PUBLIC function grants in control, want 0", n)
	}
	if _, err := p.Exec(ctx, ins, "harness:r1", "harness", "s"); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t) // Open resets through Store.Truncate
	if n := count(t, st.Pool(), ctx, `SELECT count(*) FROM control.ingested_events`); n != 0 {
		t.Fatalf("%d ingested events after the test reset, want 0", n)
	}
	// The reset re-armed the triggers.
	if _, err := st.Pool().Exec(ctx, ins, "harness:r2", "harness", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `DELETE FROM control.ingested_events`); code(err) != "23001" {
		t.Fatalf("DELETE after the reset: %v, want refused (trigger re-armed)", err)
	}
}
