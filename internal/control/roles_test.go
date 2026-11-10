package control

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed testdata/control-roles-matrix.json
var matrixJSON []byte

type matrixFile struct {
	Tables        map[string]map[string]string
	InsertColumns map[string]map[string][]string `json:"insert_columns"`
	SchemaUsage   map[string][]string            `json:"schema_usage"`
	Database      struct{ Connect, Create, Temp bool }
	// Functions in schema public: the matrix expects EXECUTE on every function
	// except a SECURITY DEFINER one and the ones with an explicit revoke, plus
	// the two literal exceptions below (the known existing state of Fabric).
	DefinerPublicExecute []string            `json:"definer_public_execute"`
	NondefinerDenied     []string            `json:"nondefiner_denied"`
	ControlExecute       map[string][]string `json:"control_function_execute"`
}

// setupRoles bootstraps control, applies the three roles under a unique
// prefix (roles are cluster-wide) and removes them when the test ends.
func setupRoles(t *testing.T) (*pgxpool.Pool, context.Context, Roles) {
	t.Helper()
	p, ctx := open(t)
	var suffix string
	if err := p.QueryRow(ctx, "SELECT substr(md5(random()::text || clock_timestamp()::text), 1, 8)").Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	r := Roles{Prefix: "ctt_" + suffix}
	if _, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil {
		t.Fatalf("ApplyRoles: %v", err)
	}
	t.Cleanup(func() {
		if _, err := RemoveRoles(context.Background(), p, r, true); err != nil {
			t.Errorf("removing the test roles: %v", err)
		}
	})
	return p, ctx, r
}

// execAs runs one statement as the role (the test database user is a superuser,
// so SET ROLE works) and returns its error.
func execAs(t *testing.T, p *pgxpool.Pool, ctx context.Context, role, sql string, args ...any) error {
	t.Helper()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE "+quoteID(role)); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "RESET ROLE") //nolint:errcheck
	_, err = conn.Exec(ctx, sql, args...)
	return err
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func kinds(r Roles) map[string]string {
	return map[string]string{"export": r.Export(), "ingest": r.Ingest(), "service": r.Service()}
}

func has(privs string, want string) bool {
	for _, p := range strings.Split(privs, ",") {
		if p == want {
			return true
		}
	}
	return false
}

// goodqa#15623 AC-1 to AC-3: every cell of the privilege matrix, against the
// committed literal.
func TestControlRolesPrivilegeMatrix(t *testing.T) {
	p, ctx, r := setupRoles(t)
	var m matrixFile
	if err := json.Unmarshal(matrixJSON, &m); err != nil {
		t.Fatal(err)
	}
	rels := func(kindsIn string) [][3]string { // schema, name, oid
		rows, err := p.Query(ctx, `SELECT n.nspname, c.relname, c.oid::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname IN ('public', 'control') AND c.relkind = ANY(string_to_array($1, ','))`, kindsIn)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][3]string
		for rows.Next() {
			var a, b, c string
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, [3]string{a, b, c})
		}
		return out
	}
	tablePrivs := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	for kind, role := range kinds(r) {
		t.Run(kind, func(t *testing.T) {
			for _, rel := range rels("r,v,p") {
				full := rel[0] + "." + rel[1]
				want := m.Tables[kind][full]
				for _, priv := range tablePrivs {
					var got bool
					if err := p.QueryRow(ctx, "SELECT has_table_privilege($1, $2::oid, $3)", role, rel[2], priv).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != has(want, priv) {
						t.Errorf("%s %s on %s: got %t, want %t", kind, priv, full, got, has(want, priv))
					}
				}
				// columns: a table-level privilege covers them; INSERT may also be granted per column
				cols, err := p.Query(ctx, `SELECT attnum, attname FROM pg_attribute WHERE attrelid = $1::oid AND attnum > 0 AND NOT attisdropped`, rel[2])
				if err != nil {
					t.Fatal(err)
				}
				type col struct {
					n    int
					name string
				}
				var cs []col
				for cols.Next() {
					var c col
					if err := cols.Scan(&c.n, &c.name); err != nil {
						t.Fatal(err)
					}
					cs = append(cs, c)
				}
				cols.Close()
				ins := map[string]bool{}
				for _, c := range m.InsertColumns[kind][full] {
					ins[c] = true
				}
				for _, c := range cs {
					for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"} {
						var got bool
						if err := p.QueryRow(ctx, "SELECT has_column_privilege($1, $2::oid, $3::smallint, $4)", role, rel[2], c.n, priv).Scan(&got); err != nil {
							t.Fatal(err)
						}
						wantCol := has(want, priv) || (priv == "INSERT" && ins[c.name])
						if got != wantCol {
							t.Errorf("%s %s on column %s.%s: got %t, want %t", kind, priv, full, c.name, got, wantCol)
						}
					}
				}
			}
			for _, rel := range rels("S") {
				for _, priv := range []string{"USAGE", "SELECT", "UPDATE"} {
					var got bool
					if err := p.QueryRow(ctx, "SELECT has_sequence_privilege($1, $2::oid, $3)", role, rel[2], priv).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got {
						t.Errorf("%s has %s on sequence %s.%s, want none", kind, priv, rel[0], rel[1])
					}
				}
			}
			// functions: none in control; no SECURITY DEFINER function in public.
			frows, err := p.Query(ctx, `SELECT n.nspname, p.proname, p.oid::text, p.prosecdef FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname IN ('public', 'control') AND p.prokind = 'f'`)
			if err != nil {
				t.Fatal(err)
			}
			type fn struct {
				schema, name, oid string
				definer           bool
			}
			var fns []fn
			for frows.Next() {
				var f fn
				if err := frows.Scan(&f.schema, &f.name, &f.oid, &f.definer); err != nil {
					t.Fatal(err)
				}
				fns = append(fns, f)
			}
			frows.Close()
			for _, f := range fns {
				var got bool
				if err := p.QueryRow(ctx, "SELECT has_function_privilege($1, $2::oid, 'EXECUTE')", role, f.oid).Scan(&got); err != nil {
					t.Fatal(err)
				}
				want := f.schema == "public" && !f.definer && !contains(m.NondefinerDenied, f.name)
				if f.schema == "public" && f.definer && contains(m.DefinerPublicExecute, f.name) {
					want = true
				}
				if f.schema == "control" && contains(m.ControlExecute[kind], f.name) {
					want = true
				}
				if got != want {
					t.Errorf("%s EXECUTE on %s.%s (security definer %t): got %t, want %t", kind, f.schema, f.name, f.definer, got, want)
				}
			}
			usage := map[string]bool{}
			for _, s := range m.SchemaUsage[kind] {
				usage[s] = true
			}
			for _, s := range []string{"public", "control"} {
				var u, c bool
				if err := p.QueryRow(ctx, "SELECT has_schema_privilege($1, $2, 'USAGE'), has_schema_privilege($1, $2, 'CREATE')", role, s).Scan(&u, &c); err != nil {
					t.Fatal(err)
				}
				if u != usage[s] || c {
					t.Errorf("%s schema %s: USAGE %t (want %t), CREATE %t (want false)", kind, s, u, usage[s], c)
				}
			}
			var conn, create, temp bool
			if err := p.QueryRow(ctx, "SELECT has_database_privilege($1, current_database(), 'CONNECT'), has_database_privilege($1, current_database(), 'CREATE'), has_database_privilege($1, current_database(), 'TEMP')", role).Scan(&conn, &create, &temp); err != nil {
				t.Fatal(err)
			}
			if conn != m.Database.Connect || create != m.Database.Create || temp != m.Database.Temp {
				t.Errorf("%s database: CONNECT %t CREATE %t TEMP %t, want %t %t %t", kind, conn, create, temp, m.Database.Connect, m.Database.Create, m.Database.Temp)
			}
		})
	}
	// no default privileges for these roles or in schema control
	if n := count(t, p, ctx, `SELECT count(*) FROM pg_default_acl d WHERE d.defaclnamespace = 'control'::regnamespace
		OR d.defaclrole IN (SELECT oid FROM pg_roles WHERE rolname = ANY($1))`, r.Names()); n != 0 {
		t.Errorf("%d default privilege entries for the control roles or schema, want 0", n)
	}
	problems, _, err := VerifyRoles(ctx, p, r)
	if err != nil || len(problems) > 0 {
		t.Errorf("VerifyRoles: %v %v, want clean", problems, err)
	}
	// write attempts per role, as the role
	t.Run("export-writes", func(t *testing.T) {
		for _, q := range []string{
			`INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ('m', 1, 'K', '{}')`,
			`UPDATE events SET actor = 'x'`,
			`TRUNCATE outbox`,
			`SELECT count(*) FROM control.ingested_events`,
			`SELECT nextval('control.ingested_events_seq_seq')`,
		} {
			if err := execAs(t, p, ctx, r.Export(), q); err == nil || code(err) != "42501" && code(err) != "42P01" {
				t.Errorf("export role %q: %v, want permission denied", q, err)
			}
		}
	})
	t.Run("service-source-writes", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE control.ingested_events SET summary = 'x'`,
			`DELETE FROM control.mc_chat_mirror`,
			`TRUNCATE control.ingested_events`,
			`INSERT INTO control.allowed_sources (source) VALUES ('evil')`,
			`SELECT count(*) FROM control.bootstrap_ledger`,
			`INSERT INTO control.bootstrap_ledger (version, sha256) VALUES (99, 'x')`,
		} {
			if err := execAs(t, p, ctx, r.Service(), q); code(err) != "42501" {
				t.Errorf("service role %q: %v, want permission denied", q, err)
			}
		}
	})
}

// A table added to control after the grants is unreachable until named, and no
// default privileges exist for it.
func TestNewControlTableUnreachable(t *testing.T) {
	p, ctx, r := setupRoles(t)
	if _, err := p.Exec(ctx, `CREATE TABLE control.zz_new (a int)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Exec(context.Background(), `DROP TABLE IF EXISTS control.zz_new`) }) //nolint:errcheck
	for kind, role := range kinds(r) {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var got bool
			if err := p.QueryRow(ctx, "SELECT has_table_privilege($1, 'control.zz_new', $2)", role, priv).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got {
				t.Errorf("%s has %s on a new control table", kind, priv)
			}
		}
	}
	// re-applying the grants does not reach it either (grants are by name)
	if _, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := p.QueryRow(ctx, "SELECT has_table_privilege($1, 'control.zz_new', 'SELECT')", r.Service()).Scan(&got); err != nil || got {
		t.Fatalf("service SELECT on a new table after re-apply: %v %v, want denied", got, err)
	}
}

const ingestEv = `INSERT INTO control.ingested_events (id, ts, kind, source, summary) VALUES ($1, now(), 'wal', $2, 's')`

// D1 to D3: what the ingest role can and cannot do.
func TestIngestRoleColumns(t *testing.T) {
	p, ctx, r := setupRoles(t)
	ing := r.Ingest()
	t.Run("insert-works", func(t *testing.T) {
		if err := execAs(t, p, ctx, ing, ingestEv, "harness:ok1", "harness"); err != nil {
			t.Fatalf("a plain insert: %v", err)
		}
		if err := execAs(t, p, ctx, ing, ingestEv+` ON CONFLICT DO NOTHING`, "harness:ok1", "harness"); err != nil {
			t.Fatalf("ON CONFLICT DO NOTHING (no target, so no SELECT right is needed): %v", err)
		}
		if err := execAs(t, p, ctx, ing, `INSERT INTO control.mc_chat_mirror (id, state, ts, mc_ts, sender, body) VALUES ('mc1', 'queued', now(), now(), 'main', 'hello') ON CONFLICT DO NOTHING`); err != nil {
			t.Fatalf("mirror insert: %v", err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM control.ingested_events WHERE id = 'harness:ok1'`); n != 1 {
			t.Fatalf("%d rows, want 1", n)
		}
	})
	t.Run("overriding-seq", func(t *testing.T) {
		q := `INSERT INTO control.ingested_events (seq, id, ts, kind, source, summary) OVERRIDING SYSTEM VALUE VALUES (5, 'harness:seq', now(), 'wal', 'harness', 's')`
		if err := execAs(t, p, ctx, ing, q); code(err) != "42501" {
			t.Fatalf("seq with OVERRIDING SYSTEM VALUE: %v, want permission denied", err)
		}
		q = `INSERT INTO control.mc_chat_mirror (seq, id, state, ts, mc_ts, sender, body) OVERRIDING SYSTEM VALUE VALUES (5, 'mcseq', 'queued', now(), now(), 'main', 'b')`
		if err := execAs(t, p, ctx, ing, q); code(err) != "42501" {
			t.Fatalf("mirror seq override: %v, want permission denied", err)
		}
	})
	t.Run("received-at", func(t *testing.T) {
		q := `INSERT INTO control.ingested_events (id, ts, kind, source, summary, received_at) VALUES ('harness:ra', now(), 'wal', 'harness', 's', '1999-01-01')`
		if err := execAs(t, p, ctx, ing, q); code(err) != "42501" {
			t.Fatalf("explicit received_at: %v, want permission denied", err)
		}
	})
	t.Run("on-conflict-update", func(t *testing.T) {
		if err := execAs(t, p, ctx, ing, ingestEv+` ON CONFLICT (id) DO UPDATE SET summary = 'x'`, "harness:ok1", "harness"); code(err) != "42501" {
			t.Fatalf("ON CONFLICT DO UPDATE: %v, want permission denied", err)
		}
	})
	t.Run("reads", func(t *testing.T) {
		for _, q := range []string{
			`SELECT 1 FROM outbox`, `SELECT 1 FROM events`, `SELECT 1 FROM control.bootstrap_ledger`,
			`SELECT 1 FROM control.mc_chat_mirror`, `SELECT 1 FROM control.ingested_events`, `SELECT 1 FROM control.mirror_content`,
		} {
			if err := execAs(t, p, ctx, ing, q); code(err) != "42501" {
				t.Errorf("ingest role %q: %v, want permission denied", q, err)
			}
		}
	})
	t.Run("fabric-origin", func(t *testing.T) {
		if err := execAs(t, p, ctx, ing, ingestEv, "fabric:1", "fabric"); code(err) != "23514" {
			t.Fatalf("a row of Fabric origin: %v, want a check violation", err)
		}
		if err := execAs(t, p, ctx, ing, ingestEv, "fabric:2", "harness"); code(err) != "23514" {
			t.Fatalf("Fabric id under another source: %v, want a check violation", err)
		}
	})
	t.Run("unlisted-source", func(t *testing.T) {
		for _, src := range []string{"fabric-x", "other", "harness2", "mission-control2"} {
			if err := execAs(t, p, ctx, ing, ingestEv, src+":e", src); code(err) != "23514" {
				t.Errorf("source %q: %v, want refused by the allowlist trigger", src, err)
			}
		}
		if err := execAs(t, p, ctx, ing, ingestEv, "mission-control:e1", "mission-control"); err != nil {
			t.Errorf("a listed source: %v", err)
		}
	})
}

// AC-10: the control roles never touch the fabric roles or settings.
func TestControlRolesLeaveSchedulerRole(t *testing.T) {
	p, ctx, r := setupRoles(t)
	snap := func() string {
		var s string
		if err := p.QueryRow(ctx, `SELECT coalesce((SELECT value FROM fabric_settings WHERE key = 'scheduler_role'), '-') || '|' ||
			(SELECT md5(coalesce(string_agg(rolname || coalesce(rolconfig::text, '') || rolcanlogin::text, ',' ORDER BY rolname), '')) FROM pg_roles WHERE rolname LIKE 'fabric\_%')
			|| '|' || (SELECT md5(coalesce(string_agg(c.relname || coalesce(c.relacl::text, ''), ',' ORDER BY c.relname), '')) FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r')`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := p.Exec(ctx, `INSERT INTO fabric_settings (key, value) VALUES ('scheduler_role', 'fabric_live_scheduler') ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	before := snap()
	if _, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil {
		t.Fatal(err)
	}
	if after := snap(); after != before {
		t.Fatalf("applying the control roles changed fabric state:\n before %s\n after  %s", before, after)
	}
	var v string
	if err := p.QueryRow(ctx, `SELECT value FROM fabric_settings WHERE key = 'scheduler_role'`).Scan(&v); err != nil || v != "fabric_live_scheduler" {
		t.Fatalf("scheduler_role %q %v, want fabric_live_scheduler", v, err)
	}
}

// AC-8: idempotent, and remove keeps every row.
func TestControlRolesIdempotentAndRemove(t *testing.T) {
	p, ctx, r := setupRoles(t)
	t.Run("idempotent", func(t *testing.T) {
		changed, err := ApplyRoles(ctx, p, r, Passwords{})
		if err != nil || changed {
			t.Fatalf("second apply: changed %t err %v, want unchanged", changed, err)
		}
	})
	if _, err := p.Exec(ctx, ingestEv, "harness:keep", "harness"); err != nil {
		t.Fatal(err)
	}
	t.Run("remove-plan-only", func(t *testing.T) {
		plan, err := RemoveRoles(ctx, p, r, false)
		if err != nil || len(plan) == 0 {
			t.Fatalf("plan: %v %v", plan, err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)`, r.Names()); n != 3 {
			t.Fatalf("%d roles after a plan-only run, want 3", n)
		}
	})
	t.Run("owned-object-refuses", func(t *testing.T) {
		if _, err := p.Exec(ctx, "ALTER ROLE "+quoteID(r.Service())+" CREATEROLE"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `CREATE TABLE control.zz_owned (a int)`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, "ALTER TABLE control.zz_owned OWNER TO "+quoteID(r.Service())); err != nil {
			t.Fatal(err)
		}
		_, err := RemoveRoles(ctx, p, r, true)
		if err == nil || !strings.Contains(err.Error(), "owns") {
			t.Fatalf("remove with an owned table: %v, want refusal", err)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)`, r.Names()); n != 3 {
			t.Fatalf("%d roles after a refused remove, want 3", n)
		}
		if _, err := p.Exec(ctx, "DROP TABLE control.zz_owned"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, "ALTER ROLE "+quoteID(r.Service())+" NOCREATEROLE"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("remove-with-session", func(t *testing.T) {
		conn, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hijacked := false
		defer func() { // a failed assertion must not leave the connection checked out (the pool Close would wait for it)
			if !hijacked {
				conn.Release()
			}
		}()
		if _, err := conn.Exec(ctx, "SET ROLE "+quoteID(r.Ingest())); err != nil {
			t.Fatal(err)
		}
		if _, err := RemoveRoles(ctx, p, r, true); err != nil {
			t.Fatalf("remove with a live session: %v", err)
		}
		hijacked = true
		conn.Hijack().Close(ctx) //nolint:errcheck // its backend was ended by the removal
		if n := count(t, p, ctx, `SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)`, r.Names()); n != 0 {
			t.Fatalf("%d roles left after remove, want 0", n)
		}
	})
	t.Run("remove-keeps-rows", func(t *testing.T) {
		if n := count(t, p, ctx, `SELECT count(*) FROM control.ingested_events WHERE id = 'harness:keep'`); n != 1 {
			t.Fatalf("%d rows after remove, want 1", n)
		}
		if n := count(t, p, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'control'`); n < 9 {
			t.Fatalf("%d control tables after remove, want the schema intact", n)
		}
	})
	t.Run("resume", func(t *testing.T) {
		// roles were removed above; a fresh apply converges to the same state
		if _, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil {
			t.Fatal(err)
		}
		changed, err := ApplyRoles(ctx, p, r, Passwords{})
		if err != nil || changed {
			t.Fatalf("apply after remove then apply again: changed %t err %v", changed, err)
		}
	})
}

// AC-13: password hygiene: roles are LOGIN with the right attributes when a
// password is given and the password leaves no trace in the places we can read.
func TestControlRolesPasswordHygiene(t *testing.T) {
	p, ctx := open(t)
	r := Roles{Prefix: fmt.Sprintf("ctt_pw%d", count(t, p, ctx, "SELECT (random() * 1000000)::int"))}
	pw := Passwords{Export: "Zq9-export-0123456789abcdef", Ingest: "Zq9-ingest-0123456789abcdef", Service: "Zq9-service-0123456789abcdef"}
	if _, err := ApplyRoles(ctx, p, r, pw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveRoles(context.Background(), p, r, true) }) //nolint:errcheck
	for _, n := range r.Names() {
		var login, super, inherit bool
		if err := p.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolinherit FROM pg_roles WHERE rolname = $1`, n).Scan(&login, &super, &inherit); err != nil {
			t.Fatal(err)
		}
		if !login || super || inherit {
			t.Errorf("role %s: login %t super %t inherit %t, want login, no superuser, noinherit", n, login, super, inherit)
		}
	}
	// pg_stat_activity must not show a password literal.
	for _, secret := range []string{pw.Export, pw.Ingest, pw.Service} {
		if n := count(t, p, ctx, `SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%' || $1 || '%'`, secret); n != 0 {
			t.Errorf("a password literal is visible in pg_stat_activity (%d sessions)", n)
		}
		if tracked := count(t, p, ctx, `SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'`); tracked > 0 {
			if n := count(t, p, ctx, `SELECT count(*) FROM pg_stat_statements WHERE query LIKE '%' || $1 || '%'`, secret); n != 0 {
				t.Errorf("a password literal is stored in pg_stat_statements (%d)", n)
			}
		}
	}
	// the error path never echoes the statement
	_, err := ApplyRoles(ctx, p, Roles{Prefix: "ctt_pw_bad"}, Passwords{Export: "x'; DROP"})
	if err != nil && strings.Contains(err.Error(), "DROP") {
		t.Errorf("an error echoed a password-bearing statement: %v", err)
	}
	if _, err := RemoveRoles(ctx, p, Roles{Prefix: "ctt_pw_bad"}, true); err != nil {
		t.Errorf("cleanup of the bad-prefix roles: %v", err)
	}
}
