package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ownedCount is the number of objects (relations, schemas, functions, types,
// databases) a role owns; remove mode refuses when it is above zero, because
// dropping an owner would drop its objects.
func ownedCount(ctx context.Context, pool *pgxpool.Pool, role string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class c WHERE c.relowner = r.oid) +
		(SELECT count(*) FROM pg_namespace n WHERE n.nspowner = r.oid) +
		(SELECT count(*) FROM pg_proc p WHERE p.proowner = r.oid) +
		(SELECT count(*) FROM pg_type t WHERE t.typowner = r.oid) +
		(SELECT count(*) FROM pg_database d WHERE d.datdba = r.oid)
		FROM pg_roles r WHERE r.rolname = $1`, role).Scan(&n)
	return n, err
}

// RemoveRoles drops the three control roles, in a stated order and with REVOKE
// and DROP ROLE only (never DROP OWNED, which would drop an owned object and
// its rows). Before changing anything it refuses when a control role owns
// anything. Without confirm it only returns the plan. With confirm it ends the
// roles' sessions, revokes their grants in the current database and drops the
// roles; a role that still holds grants in another database makes DROP ROLE
// fail, and that error is returned (run it in that database first). It never
// drops the schema, its tables or any row.
func RemoveRoles(ctx context.Context, pool *pgxpool.Pool, r Roles, confirm bool) ([]string, error) {
	if !rolePrefix.MatchString(r.Prefix) {
		return nil, fmt.Errorf("role prefix %q", r.Prefix)
	}
	var existing []string
	for _, n := range r.Names() {
		var found bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", n).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		owned, err := ownedCount(ctx, pool, n)
		if err != nil {
			return nil, err
		}
		if owned > 0 {
			return nil, fmt.Errorf("remove control roles refused: role %s owns %d object(s); nothing was changed", n, owned)
		}
		existing = append(existing, n)
	}
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return nil, err
	}
	var plan []string
	for _, n := range existing {
		plan = append(plan,
			"end sessions of "+n,
			fmt.Sprintf("REVOKE ALL on control, public and CONNECT on %s from %s", db, n),
			"DROP ROLE "+n)
	}
	if len(existing) == 0 || !confirm {
		return plan, nil
	}
	for _, n := range existing {
		if _, err := pool.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1 AND pid <> pg_backend_pid()", n); err != nil {
			return plan, fmt.Errorf("remove control roles: cannot end the sessions of %s: %w", n, redactErr(err))
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return plan, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", rolesLock); err != nil {
		return plan, fmt.Errorf("remove control roles: %w", redactErr(err))
	}
	for _, n := range existing {
		q := quoteID(n)
		for _, s := range []string{
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA control FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA control FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON ALL FUNCTIONS IN SCHEMA control FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON SCHEMA control FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON SCHEMA public FROM %s", q),
			fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s", quoteID(db), q),
			"DROP ROLE " + q,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return plan, fmt.Errorf("remove control roles: %s: %w", strings.SplitN(s, " ", 3)[0]+" for "+n, redactErr(err))
			}
		}
	}
	return plan, tx.Commit(ctx)
}

// VerifyRoles reports every way the control roles differ from the design, read
// only and without connecting to any other database: role attributes, owned
// objects, memberships (the creating admin's own membership is allowed),
// CREATE on schemas public and control, CREATE on the database, and for every
// other database the CONNECT right read from the shared catalog (such databases
// are reported as "unchecked, not connected"). An empty result means clean.
func VerifyRoles(ctx context.Context, pool *pgxpool.Pool, r Roles) (problems, notes []string, err error) {
	if !rolePrefix.MatchString(r.Prefix) {
		return nil, nil, fmt.Errorf("role prefix %q", r.Prefix)
	}
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return nil, nil, err
	}
	for _, n := range r.Names() {
		var super, createdb, createrole, repl, bypass, inherit bool
		err := pool.QueryRow(ctx, `SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolinherit FROM pg_roles WHERE rolname = $1`, n).
			Scan(&super, &createdb, &createrole, &repl, &bypass, &inherit)
		if err != nil {
			problems = append(problems, fmt.Sprintf("role %s is missing", n))
			continue
		}
		for what, bad := range map[string]bool{"SUPERUSER": super, "CREATEDB": createdb, "CREATEROLE": createrole, "REPLICATION": repl, "BYPASSRLS": bypass, "INHERIT": inherit} {
			if bad {
				problems = append(problems, fmt.Sprintf("role %s has %s", n, what))
			}
		}
		if owned, err := ownedCount(ctx, pool, n); err != nil {
			return nil, nil, err
		} else if owned > 0 {
			problems = append(problems, fmt.Sprintf("role %s owns %d object(s)", n, owned))
		}
		var members int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid) +
			(SELECT count(*) FROM pg_auth_members m JOIN pg_roles u ON u.oid = m.member WHERE m.roleid = r.oid AND u.rolname <> current_user)
			FROM pg_roles r WHERE r.rolname = $1`, n).Scan(&members); err != nil {
			return nil, nil, err
		}
		if members > 0 {
			problems = append(problems, fmt.Sprintf("role %s has %d role membership(s) other than the creating admin's", n, members))
		}
		for _, s := range []struct{ schema, priv string }{{"public", "CREATE"}, {"control", "CREATE"}} {
			var has bool
			if err := pool.QueryRow(ctx, "SELECT has_schema_privilege($1, $2, $3)", n, s.schema, s.priv).Scan(&has); err != nil {
				return nil, nil, err
			}
			if has {
				problems = append(problems, fmt.Sprintf("role %s may %s in schema %s", n, s.priv, s.schema))
			}
		}
		var dbcreate bool
		if err := pool.QueryRow(ctx, "SELECT has_database_privilege($1, $2, 'CREATE')", n, db).Scan(&dbcreate); err != nil {
			return nil, nil, err
		}
		if dbcreate {
			problems = append(problems, fmt.Sprintf("role %s may CREATE in database %s", n, db))
		}
		rows, err := pool.Query(ctx, `SELECT datname, has_database_privilege($1, datname, 'CONNECT') FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname <> $2 ORDER BY datname`, n, db)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var name string
			var connect bool
			if err := rows.Scan(&name, &connect); err != nil {
				rows.Close()
				return nil, nil, err
			}
			notes = append(notes, fmt.Sprintf("other_db_rights: database %s: unchecked, not connected; role %s CONNECT=%t (read from the shared catalog)", name, n, connect))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return problems, notes, nil
}

// ErrVerify is returned by the CLI when VerifyRoles finds a problem.
var ErrVerify = errors.New("control roles verification found problems")
