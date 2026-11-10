package control

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/storetest"
)

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

func kinds(r Roles) map[string]string {
	return map[string]string{"export": r.Export(), "ingest": r.Ingest(), "service": r.Service()}
}

// goodqa#15623 AC-1 to AC-3: every cell of the privilege matrix, against the
// committed literal (roles-matrix.json, compared by matrixProblems, the same code
// `fab db roles --control-only --verify` runs).
func TestControlRolesPrivilegeMatrix(t *testing.T) {
	p, ctx, r := setupRoles(t)
	for kind, role := range kinds(r) {
		problems, err := matrixProblems(ctx, p, kind, role)
		if err != nil {
			t.Fatal(err)
		}
		for _, pr := range problems {
			t.Errorf("%s", pr)
		}
	}
	problems, _, err := VerifyRoles(ctx, p, r)
	if err != nil || len(problems) > 0 {
		t.Errorf("VerifyRoles: %v %v, want clean", problems, err)
	}
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
	_, err := ApplyRoles(ctx, p, Roles{Prefix: r.Prefix + "_bad"}, Passwords{Export: "x'; DROP"})
	if err != nil && strings.Contains(err.Error(), "DROP") {
		t.Errorf("an error echoed a password-bearing statement: %v", err)
	}
	if _, err := RemoveRoles(ctx, p, Roles{Prefix: r.Prefix + "_bad"}, true); err != nil {
		t.Errorf("cleanup of the bad-prefix roles: %v", err)
	}
}

// goodqa#15623 AC-7 and H2 of the code review: VerifyRoles compares the live
// grants with the matrix, so extra privileges (which the role attribute checks do
// not see) are reported.
func TestVerifyRolesDetectsGrantDrift(t *testing.T) {
	p, ctx, r := setupRoles(t)
	problems, _, err := VerifyRoles(ctx, p, r)
	if err != nil || len(problems) != 0 {
		t.Fatalf("clean roles: %v %v", problems, err)
	}
	for name, c := range map[string]struct{ grant, revoke, want string }{
		"ingest-select-mirror":    {"GRANT SELECT ON control.mc_chat_mirror TO " + r.Ingest(), "REVOKE SELECT ON control.mc_chat_mirror FROM " + r.Ingest(), "ingest SELECT on control.mc_chat_mirror"},
		"ingest-mirror-content":   {"GRANT SELECT, INSERT ON control.mirror_content TO " + r.Ingest(), "REVOKE ALL ON control.mirror_content FROM " + r.Ingest(), "ingest INSERT on control.mirror_content"},
		"export-missions":         {"GRANT SELECT ON public.missions TO " + r.Export(), "REVOKE SELECT ON public.missions FROM " + r.Export(), "export SELECT on public.missions"},
		"export-control-usage":    {"GRANT USAGE ON SCHEMA control TO " + r.Export(), "REVOKE USAGE ON SCHEMA control FROM " + r.Export(), "export schema control"},
		"service-allowed-sources": {"GRANT INSERT ON control.allowed_sources TO " + r.Service(), "REVOKE INSERT ON control.allowed_sources FROM " + r.Service(), "service INSERT on control.allowed_sources"},
		"service-sequence":        {"GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO " + r.Service(), "REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM " + r.Service(), "service has USAGE on sequence"},
	} {
		if _, err := p.Exec(ctx, c.grant); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		problems, _, err := VerifyRoles(ctx, p, r)
		found := false
		for _, pr := range problems {
			if strings.Contains(pr, c.want) {
				found = true
			}
		}
		if err != nil || !found {
			t.Errorf("%s: VerifyRoles did not report %q (problems %v, err %v)", name, c.want, problems, err)
		}
		if _, err := p.Exec(ctx, c.revoke); err != nil {
			t.Fatalf("%s revoke: %v", name, err)
		}
	}
	if problems, _, err := VerifyRoles(ctx, p, r); err != nil || len(problems) != 0 {
		t.Fatalf("after reverting the drift: %v %v", problems, err)
	}
}

// goodqa#15623 AC-13 and H1 of the code review: a caller that cannot silence the
// server log (a non-superuser) is refused before any password is sent when the log
// would record a failing ALTER ROLE statement.
func TestApplyRolesRefusesLoudLogging(t *testing.T) {
	p, ctx := open(t)
	var lmes, ls, lmd string
	if err := p.QueryRow(ctx, "SELECT current_setting('log_min_error_statement'), current_setting('log_statement'), current_setting('log_min_duration_statement')").Scan(&lmes, &ls, &lmd); err != nil {
		t.Fatal(err)
	}
	if lmes == "panic" && ls == "none" && lmd == "-1" {
		t.Skip("the cluster already runs quiet logging: the refusal path cannot be reached")
	}
	var suffix string
	if err := p.QueryRow(ctx, "SELECT substr(md5(random()::text), 1, 8)").Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	adm := "ctt_adm_" + suffix
	const admPassword = "AdmPw-0123456789-abcdef"
	if _, err := p.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN CREATEROLE PASSWORD '%s'", adm, admPassword)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Exec(context.Background(), "REVOKE ALL ON SCHEMA control FROM "+adm+"; DROP ROLE IF EXISTS "+adm) }) //nolint:errcheck
	if _, err := p.Exec(ctx, "GRANT USAGE ON SCHEMA control TO "+adm+"; GRANT SELECT ON control.ingested_events TO "+adm); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(adm, admPassword)
	np, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer np.Close()
	r := Roles{Prefix: "ctt_" + suffix}
	const secret = "Zq9-refused-0123456789abcdef"
	_, err = ApplyRoles(ctx, np, r, Passwords{Export: secret})
	if err == nil || !strings.Contains(err.Error(), "refusing to send a password") {
		t.Fatalf("ApplyRoles as a non-superuser under loud logging: %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the refusal echoed the password")
	}
	if n := count(t, p, ctx, `SELECT count(*) FROM pg_roles WHERE rolname LIKE $1`, r.Prefix+"%"); n != 0 {
		t.Fatalf("%d roles were created despite the refusal", n)
	}
}
