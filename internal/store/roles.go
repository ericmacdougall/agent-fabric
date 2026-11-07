package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Roles names the database roles that replace the single superuser
// (goodqa#14929): <prefix>_scheduler (NOLOGIN, marks the roles that may
// write the scheduler events, 0016), <prefix>_app (fabd and fab: data
// access only, a scheduler member) and <prefix>_runner (what a runner and
// its jobs connect as: data access without the dispatch-gate tables). No
// role here owns a table, may TRUNCATE, or may CREATE in the schema, so
// the triggers hold against a misbehaving client. Migrations keep running
// as the owner.
type Roles struct{ Prefix string }

func (r Roles) App() string       { return r.Prefix + "_app" }
func (r Roles) Runner() string    { return r.Prefix + "_runner" }
func (r Roles) Scheduler() string { return r.Prefix + "_scheduler" }

var rolePrefix = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// runnerWrites is everything the runner role may write (red-team H1 on
// 193c0a1: it had DML on every table, so it could end its own attempt and
// free its hold). A runner requests jobs and records their exit, submits
// and dispatches effects, and raises asks; 0017 narrows jobs and effects
// further. It reads everything.
// goodqa#15082 F0: no INSERT on jobs (host jobs are disabled); UPDATE stays
// for a legacy watcher recording its exit.
var runnerWrites = map[string]string{"jobs": "UPDATE", "effects": "INSERT, UPDATE", "asks": "INSERT", "effect_dispatches": "INSERT"}

// RoleTimeout bounds every statement and idle transaction of the app and
// runner roles (goodqa#14929 item 3; also bounds goodqa#14925).
const RoleTimeout = "60s"

// applyRolesFault is a test seam: when set, ApplyRoles calls it before each
// statement and stops on its error. Always nil outside tests.
var applyRolesFault func(stmt string) error

// ApplyRoles creates or updates the roles and their grants. It runs as the
// owner (or an administrator) after Migrate and is idempotent; rerun it
// after every migration so new tables are granted. An empty password makes
// that role NOLOGIN (tests reach it through SET ROLE). Passwords are never
// logged or returned.
func (s *Store) ApplyRoles(ctx context.Context, r Roles, appPassword, runnerPassword string) error {
	if !rolePrefix.MatchString(r.Prefix) {
		return fmt.Errorf("role prefix %q", r.Prefix)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	quote := func(v string) (string, error) {
		q, err := conn.Conn().PgConn().EscapeString(v)
		return "'" + q + "'", err
	}
	login := func(pw string) (string, error) {
		if pw == "" {
			return "NOLOGIN PASSWORD NULL", nil
		}
		q, err := quote(pw)
		return "LOGIN PASSWORD " + q, err
	}
	appLogin, err := login(appPassword)
	if err != nil {
		return err
	}
	runnerLogin, err := login(runnerPassword)
	if err != nil {
		return err
	}
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return err
	}
	id := func(n string) string { return pgx.Identifier{n}.Sanitize() }
	sched, app, runner := id(r.Scheduler()), id(r.App()), id(r.Runner())
	stmts := []string{}
	for _, c := range []struct{ name, opts string }{{r.Scheduler(), "NOLOGIN"}, {r.App(), appLogin}, {r.Runner(), runnerLogin}} {
		stmts = append(stmts, fmt.Sprintf(`DO $do$ BEGIN CREATE ROLE %s NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $do$`, id(c.name)),
			fmt.Sprintf("ALTER ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION %s", id(c.name), c.opts))
	}
	for _, rl := range []string{app, runner} {
		stmts = append(stmts,
			fmt.Sprintf("ALTER ROLE %s SET statement_timeout = '%s'", rl, RoleTimeout),
			fmt.Sprintf("ALTER ROLE %s SET idle_in_transaction_session_timeout = '%s'", rl, RoleTimeout),
			fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", id(db), rl),
			fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", rl),
			fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", rl),
		)
	}
	stmts = append(stmts,
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", app),
		fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE ON fabric_settings FROM %s", app),
		// goodqa#15163 (Kimi K5): only the owner writes the migration ledger.
		fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON fabric_schema FROM %s, %s", app, runner),
		fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public FROM %s", runner),
		fmt.Sprintf("GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s", runner),
	)
	for tb, privs := range runnerWrites {
		stmts = append(stmts, fmt.Sprintf("GRANT %s ON %s TO %s", privs, id(tb), runner))
	}
	schedRole, err := quote(r.Scheduler())
	if err != nil {
		return err
	}
	stmts = append(stmts,
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("REVOKE %s FROM %s", sched, runner),
		fmt.Sprintf("GRANT %s TO %s", sched, app),
		"INSERT INTO fabric_settings (key, value) VALUES ('scheduler_role', "+schedRole+") ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
	)
	// goodqa#15159: all or nothing, one run at a time. A run that stopped
	// part way used to leave, for example, the app granted every table but
	// not yet revoked from fabric_settings. Role DDL and grants are
	// transactional in Postgres.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('fabric.apply_roles'))"); err != nil {
		return fmt.Errorf("apply roles: %w", redactPassword(err))
	}
	for _, q := range stmts {
		if applyRolesFault != nil {
			if err := applyRolesFault(q); err != nil {
				return fmt.Errorf("apply roles: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("apply roles: %w", redactPassword(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("apply roles: %w", redactPassword(err))
	}
	return nil
}

// redactPassword drops the statement text from an error, which could hold
// a password.
func redactPassword(err error) error {
	var pe interface{ SQLState() string }
	if errors.As(err, &pe) {
		return fmt.Errorf("sqlstate %s", pe.SQLState())
	}
	return errors.New("statement failed")
}

// CheckRunnerRole refuses a runner connection that could act as the
// scheduler (red-team G2): a superuser, a scheduler member, a role with an
// owner's privileges, one that may CREATE in the schema, or one holding any
// write that runnerWrites does not list. goodqa#15167 (Kimi K11): writes
// are checked on every table and view, including column-level INSERT,
// UPDATE and REFERENCES grants, and DELETE, TRUNCATE and TRIGGER. Run it on
// a store opened with the runner's DSN.
func (s *Store) CheckRunnerRole(ctx context.Context) error {
	var who string
	var super, owner, sched, create bool
	err := s.pool.QueryRow(ctx, `SELECT current_user,
		(SELECT rolsuper FROM pg_roles WHERE rolname = current_user),
		EXISTS (SELECT 1 FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace
		        AND pg_has_role(current_user, c.relowner, 'USAGE'))
		OR pg_has_role(current_user, (SELECT nspowner FROM pg_namespace WHERE nspname = 'public'), 'USAGE'),
		COALESCE((SELECT pg_has_role(current_user, value, 'MEMBER') FROM fabric_settings
		          WHERE key = 'scheduler_role' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = value)), false),
		has_schema_privilege(current_user, 'public', 'CREATE')`).Scan(&who, &super, &owner, &sched, &create)
	if err != nil {
		return err
	}
	switch {
	case super:
		return fmt.Errorf("runner role %s is a superuser", who)
	case owner:
		return fmt.Errorf("runner role %s has an owner's privileges in the schema", who)
	case sched:
		return fmt.Errorf("runner role %s is a scheduler member", who)
	case create:
		return fmt.Errorf("runner role %s may create objects in the schema", who)
	}
	rows, err := s.pool.Query(ctx, `SELECT c.relname, p.priv FROM pg_class c
		CROSS JOIN unnest(ARRAY['INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'REFERENCES', 'TRIGGER']) AS p(priv)
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p', 'v', 'f')
		AND CASE WHEN p.priv IN ('INSERT', 'UPDATE', 'REFERENCES')
		         THEN has_any_column_privilege(current_user, c.oid, p.priv)
		         ELSE has_table_privilege(current_user, c.oid, p.priv) END
		ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var extra []string
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			return err
		}
		if !runnerMayWrite(table, priv) {
			extra = append(extra, table+" "+priv)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("runner role %s holds writes it must not have: %s", who, strings.Join(extra, ", "))
	}
	return nil
}

// runnerMayWrite reports whether runnerWrites grants priv on table.
func runnerMayWrite(table, priv string) bool {
	for _, p := range strings.Split(runnerWrites[table], ",") {
		if strings.TrimSpace(p) == priv {
			return true
		}
	}
	return false
}
