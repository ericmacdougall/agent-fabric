package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"

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

// gateTables are written only by the scheduler (holds, reservations,
// grants); the runner role may read them.
var gateTables = []string{"hold_commands", "reservations", "reservation_events", "resource_capacities", "dispatch_grants", "usage_receipts", "fabric_settings"}

// RoleTimeout bounds every statement and idle transaction of the app and
// runner roles (goodqa#14929 item 3; also bounds goodqa#14925).
const RoleTimeout = "60s"

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
			fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", rl),
			fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", rl),
			fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE ON fabric_settings FROM %s", rl),
		)
	}
	for _, tb := range gateTables {
		stmts = append(stmts, fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE ON %s FROM %s", id(tb), runner))
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
	for _, q := range stmts {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("apply roles: %w", redactPassword(err))
		}
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
// scheduler (red-team G2): a superuser, a scheduler member, a role with the
// owner's privileges, or one that can write the dispatch-gate tables. Run it
// on a store opened with the runner's DSN.
func (s *Store) CheckRunnerRole(ctx context.Context) error {
	var who string
	var super, owner, sched, hold bool
	err := s.pool.QueryRow(ctx, `SELECT current_user,
		(SELECT rolsuper FROM pg_roles WHERE rolname = current_user),
		pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = 'events'::regclass), 'USAGE'),
		COALESCE((SELECT pg_has_role(current_user, value, 'MEMBER') FROM fabric_settings
		          WHERE key = 'scheduler_role' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = value)), false),
		has_table_privilege(current_user, 'hold_commands', 'INSERT')`).Scan(&who, &super, &owner, &sched, &hold)
	if err != nil {
		return err
	}
	switch {
	case super:
		return fmt.Errorf("runner role %s is a superuser", who)
	case owner:
		return fmt.Errorf("runner role %s has the schema owner's privileges", who)
	case sched:
		return fmt.Errorf("runner role %s is a scheduler member", who)
	case hold:
		return fmt.Errorf("runner role %s can write hold_commands", who)
	}
	return nil
}
