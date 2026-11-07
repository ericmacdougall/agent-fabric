package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Kimi review follow-ups K5 (goodqa#15163), K6 (goodqa#15164) and K11
// (goodqa#15167), migration 0029 and the role code.

func denied(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

// K5: no client role may write the migration ledger; a forged row could
// make Migrate skip a migration or refuse a valid one.
func TestAppCannotWriteSchemaLedger(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	for _, role := range []string{r.App(), r.Runner()} {
		st := openAs(t, ctx, role)
		for _, q := range []string{
			"INSERT INTO fabric_schema (name, sha256) VALUES ('9998_forged.sql', 'x')",
			"UPDATE fabric_schema SET sha256 = NULL WHERE name = (SELECT min(name) FROM fabric_schema)",
			"DELETE FROM fabric_schema WHERE name = (SELECT min(name) FROM fabric_schema)",
		} {
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, q)
			tx.Rollback(ctx) //nolint:errcheck
			if !denied(err) {
				t.Errorf("%s: %s -> %v, want permission denied", role, q, err)
			}
		}
	}
}

// K5 on an already deployed database: 0029 revokes ledger writes from any
// role but the owner, before ApplyRoles is rerun.
func TestMigration0029RevokesLedgerWrites(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	mustExec(t, owner, ctx, "GRANT INSERT, UPDATE, DELETE, TRUNCATE ON fabric_schema TO "+r.App())
	sql := migration0029(t)
	mustExec(t, owner, ctx, sql)
	if n := count(t, owner, ctx, `SELECT count(*) FROM information_schema.role_table_grants
		WHERE table_schema = 'public' AND table_name = 'fabric_schema' AND privilege_type <> 'SELECT'
		AND grantee <> (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'public.fabric_schema'::regclass)`); n != 0 {
		t.Fatalf("%d write grants on fabric_schema survive 0029", n)
	}
	// The migration is safe to apply twice (a manual rerun).
	mustExec(t, owner, ctx, sql)
}

func migration0029(t *testing.T) string {
	t.Helper()
	names, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, "/0029_function_search_path.sql") {
			b, err := migrationFS.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatal("migration 0029_function_search_path.sql is not embedded")
	return ""
}

// K6: every function in the schema runs with a pinned search_path that
// searches pg_temp last, so a session's temporary table cannot shadow a
// table a trigger or backstop reads. New functions must pin it too.
func TestEveryFunctionPinsSearchPath(t *testing.T) {
	owner, ctx := openTest(t)
	rows, err := owner.pool.Query(ctx, `SELECT p.oid::regprocedure::text, COALESCE((
			SELECT substring(c FROM 'search_path=(.*)') FROM unnest(p.proconfig) c WHERE c LIKE 'search_path=%'), '')
		FROM pg_proc p
		WHERE p.pronamespace = 'public'::regnamespace
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var fn, path string
		if err := rows.Scan(&fn, &path); err != nil {
			t.Fatal(err)
		}
		n++
		parts := strings.Split(path, ",")
		for i := range parts {
			parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"`)
		}
		if path == "" || parts[0] != "pg_catalog" || parts[len(parts)-1] != "pg_temp" {
			t.Errorf("function %s search_path %q, want it to start with pg_catalog and end with pg_temp", fn, path)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("only %d functions found: the check is not looking at the schema", n)
	}
}

// K6 behaviour: a temporary reservations table does not hide the real
// holds from the capacity backstop.
func TestTempTableCannotShadowBackstop(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	mustExec(t, owner, ctx, "INSERT INTO resource_capacities (resource, unit, capacity, held) VALUES ('k6.probe', 'count', 10, 0)")
	app := openAs(t, ctx, r.App())
	conn, err := app.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "UPDATE public.resource_capacities SET held = 7 WHERE resource = 'k6.probe'"); err == nil {
		t.Fatal("control: the backstop accepted a held that no hold explains")
	}
	if _, err := conn.Exec(ctx, "CREATE TEMP TABLE reservations (resource text, state text, amount numeric); INSERT INTO reservations VALUES ('k6.probe', 'HELD', 7)"); err != nil {
		t.Fatalf("temp table: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE public.resource_capacities SET held = 7 WHERE resource = 'k6.probe'"); err == nil {
		t.Fatal("a temporary reservations table shadowed the backstop's read")
	}
}

// K11: CheckRunnerRole refuses any write a runner is not meant to have,
// including a column-level grant, DELETE, TRUNCATE, TRIGGER or REFERENCES,
// on any table, and CREATE on the schema.
func TestRunnerCheckRefusesExtraPrivileges(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := openAs(t, ctx, r.Runner())
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("the runner role ApplyRoles made is refused: %v", err)
	}
	for _, c := range []struct{ grant, revoke string }{
		{"GRANT UPDATE (state) ON attempts TO %s", "REVOKE UPDATE ON attempts FROM %s"},
		{"GRANT INSERT (id) ON missions TO %s", "REVOKE INSERT ON missions FROM %s"},
		{"GRANT UPDATE (value) ON fabric_settings TO %s", "REVOKE UPDATE ON fabric_settings FROM %s"},
		{"GRANT DELETE ON effects TO %s", "REVOKE DELETE ON effects FROM %s"},
		{"GRANT TRUNCATE ON jobs TO %s", "REVOKE TRUNCATE ON jobs FROM %s"},
		{"GRANT TRIGGER ON asks TO %s", "REVOKE TRIGGER ON asks FROM %s"},
		{"GRANT INSERT ON reservations TO %s", "REVOKE INSERT ON reservations FROM %s"},
		{"GRANT UPDATE ON asks TO %s", "REVOKE UPDATE ON asks FROM %s"},
		{"GRANT INSERT ON jobs TO %s", "REVOKE INSERT ON jobs FROM %s"},
		{"GRANT CREATE ON SCHEMA public TO %s", "REVOKE CREATE ON SCHEMA public FROM %s"},
	} {
		mustExec(t, owner, ctx, fmt.Sprintf(c.grant, r.Runner()))
		err := runner.CheckRunnerRole(ctx)
		mustExec(t, owner, ctx, fmt.Sprintf(c.revoke, r.Runner()))
		if err == nil {
			t.Errorf("%s: CheckRunnerRole passed", fmt.Sprintf(c.grant, "runner"))
		}
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("after revokes: %v", err)
	}
}

// ApplyRoles is all or nothing: a failure part way leaves no role and no
// grant behind (a half-applied run left the app able to write settings).
func TestApplyRolesIsAtomic(t *testing.T) {
	owner, ctx := openTest(t)
	r := Roles{Prefix: fmt.Sprintf("fabric_a%d", count(t, owner, ctx, "SELECT pg_backend_pid()"))}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			owner.pool.Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			owner.pool.Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	applyRolesFault = func(stmt string) error {
		if strings.HasPrefix(stmt, "REVOKE INSERT, UPDATE, DELETE ON fabric_settings") {
			return errors.New("injected fault")
		}
		return nil
	}
	t.Cleanup(func() { applyRolesFault = nil })
	if err := owner.ApplyRoles(ctx, r, "", ""); err == nil {
		t.Fatal("ApplyRoles ignored the injected fault")
	}
	if n := count(t, owner, ctx, "SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)", []string{r.App(), r.Runner(), r.Scheduler()}); n != 0 {
		t.Fatalf("%d roles survive a failed ApplyRoles", n)
	}
	applyRolesFault = nil
	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatalf("clean rerun: %v", err)
	}
}
