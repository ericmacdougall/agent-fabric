package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/passwordsql"
)

// Roles names the three database roles of the Control Room (goodqa#15586, spec
// goodqa#15623): PREFIX_control_export (reads outbox and events plus four joined metadata columns, writes nothing),
// PREFIX_control_ingest (INSERT on named columns of the two source tables,
// SELECT on allowed_sources) and PREFIX_control_service (reads the source
// tables and a named list of Fabric tables, writes the projection tables). They
// are separate from the fabric roles of store.Roles and never touch them.
type Roles struct{ Prefix string }

func (r Roles) Export() string  { return r.Prefix + "_control_export" }
func (r Roles) Ingest() string  { return r.Prefix + "_control_ingest" }
func (r Roles) Service() string { return r.Prefix + "_control_service" }

// Names returns the three role names.
func (r Roles) Names() []string { return []string{r.Export(), r.Ingest(), r.Service()} }

var rolePrefix = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Passwords holds the plaintext passwords (from 0600 files, never logged). An
// empty password makes that role NOLOGIN.
type Passwords struct{ Export, Ingest, Service string }

// roleTimeout bounds every statement and idle transaction of the control roles.
const roleTimeout = "60s"

// rolesLock serialises role changes (a distinct key from store's apply_roles lock).
const rolesLock = "fabric.apply_control_roles"

// Grants of the control roles. These are the single source of the grant code;
// the privilege matrix test compares the result with a literal committed file.
var (
	// exportFabric: whole-table reads; the two joins below use column grants only.
	exportFabric  = []string{"outbox", "events"}
	exportColumns = map[string][]string{"missions": {"id", "ticket"}, "outbox_cursors": {"consumer", "last_id"}}
	// serviceFabric: what the service reads of Fabric's own tables.
	serviceFabric = []string{"events", "outbox", "outbox_cursors", "missions", "attempts", "jobs", "waits"}
	// ingestEvents: the insert columns of control.ingested_events. seq,
	// received_at are left out on purpose (OVERRIDING SYSTEM VALUE must fail).
	ingestEvents = []string{"id", "ts", "kind", "source", "host", "actor", "initiative", "repo", "branch", "ref", "ticket", "summary", "severity", "count", "tenant"}
	// ingestMirror: the insert columns of control.mc_chat_mirror; seq,
	// content_sha256 and received_at are left out.
	ingestMirror = []string{"id", "state", "origin", "ts", "mc_ts", "sender", "recipient", "body"}
	// serviceRead: control tables and the view the service reads.
	serviceRead = []string{"ingested_events", "mc_chat_mirror", "mirror_states", "mirror_refusals", "allowed_sources", "mc_chat_latest"}
	// serviceWrite: the projection tables the service writes.
	serviceWrite = []string{"proj_cursor", "initiative_state", "pulses"}
)

func quoteID(n string) string { return pgx.Identifier{n}.Sanitize() }

func qualified(schema, table string) string { return pgx.Identifier{schema, table}.Sanitize() }

func columns(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteID(c)
	}
	return strings.Join(q, ", ")
}

// redactErr drops the statement text from an error, which could hold a password.
func redactErr(err error) error { return passwordsql.Safe(err) }

// ApplyRoles creates or updates the three control roles and their grants. It
// is idempotent, runs as the database owner in one transaction under an
// advisory lock, touches no fabric_* role, no fabric_settings row and no
// password of any other role, and never logs a password. The control schema must
// already exist (fab control bootstrap).
func ApplyRoles(ctx context.Context, pool *pgxpool.Pool, r Roles, pw Passwords) (changed bool, err error) {
	if !rolePrefix.MatchString(r.Prefix) {
		return false, errors.New("invalid control role prefix")
	}
	for _, item := range []struct{ role, password string }{{r.Export(), pw.Export}, {r.Ingest(), pw.Ingest}, {r.Service(), pw.Service}} {
		if !passwordsql.Valid(item.role, item.password) {
			return false, passwordsql.ErrRefused
		}
	}
	conn, err := passwordsql.Connect(ctx, pool.Config())
	if err != nil {
		return false, err
	}
	committed := false
	withPassword := pw.Export != "" || pw.Ingest != "" || pw.Service != ""
	var tx pgx.Tx
	var protection passwordsql.Protection
	key := ""
	defer func() { err = passwordsql.Close(ctx, conn, tx, protection, key, withPassword, committed, err) }()
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return false, passwordsql.Safe(err)
	}
	var haveControl bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('control.ingested_events') IS NOT NULL").Scan(&haveControl); err != nil {
		return false, passwordsql.Safe(err)
	}
	if !haveControl {
		return false, errors.New("the control schema does not exist: run fab control bootstrap first")
	}
	var stmts []string
	passwordActions := map[string]struct{ role, password string }{}
	before, err := rolesDigest(ctx, conn, r)
	if err != nil {
		return false, passwordsql.Safe(err)
	}
	for _, c := range []struct{ name, pw string }{{r.Export(), pw.Export}, {r.Ingest(), pw.Ingest}, {r.Service(), pw.Service}} {
		opts := "NOLOGIN"
		if c.pw == "" {
			opts = "NOLOGIN PASSWORD NULL"
		}
		n := quoteID(c.name)
		alter := fmt.Sprintf("ALTER ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT %s", n, opts)
		if c.pw != "" {
			passwordActions[alter] = struct{ role, password string }{c.name, c.pw}
		}
		stmts = append(stmts,
			fmt.Sprintf(`DO $do$ BEGIN CREATE ROLE %s NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $do$`, n),
			alter,
			fmt.Sprintf("ALTER ROLE %s SET statement_timeout = '%s'", n, roleTimeout),
			fmt.Sprintf("ALTER ROLE %s SET idle_in_transaction_session_timeout = '%s'", n, roleTimeout),
			fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", quoteID(db), n),
			// Start from nothing: every grant below is stated, so a re-run converges.
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA control FROM %s", n),
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s", n),
			fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA control FROM %s", n),
			fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s", n),
			fmt.Sprintf("REVOKE ALL ON ALL FUNCTIONS IN SCHEMA control FROM %s", n),
			fmt.Sprintf("REVOKE ALL ON SCHEMA control FROM %s", n),
			fmt.Sprintf("REVOKE CREATE ON SCHEMA public FROM %s", n),
			fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", n),
		)
	}
	e, ing, svc := quoteID(r.Export()), quoteID(r.Ingest()), quoteID(r.Service())
	for _, t := range exportFabric {
		stmts = append(stmts, fmt.Sprintf("GRANT SELECT ON %s TO %s", qualified("public", t), e))
	}
	for _, table := range []string{"missions", "outbox_cursors"} {
		stmts = append(stmts, fmt.Sprintf("GRANT SELECT (%s) ON %s TO %s", columns(exportColumns[table]), qualified("public", table), e))
	}
	stmts = append(stmts,
		fmt.Sprintf("GRANT USAGE ON SCHEMA control TO %s", ing),
		fmt.Sprintf("GRANT INSERT (%s) ON control.ingested_events TO %s", columns(ingestEvents), ing),
		fmt.Sprintf("GRANT INSERT (%s) ON control.mc_chat_mirror TO %s", columns(ingestMirror), ing),
		fmt.Sprintf("GRANT SELECT ON control.allowed_sources TO %s", ing),
		// The generated column content_sha256 is evaluated with the inserting
		// role's rights, so the ingest role may execute the (pure) hash function.
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION control.mirror_hash(text, text, text) TO %s", ing),
		fmt.Sprintf("GRANT USAGE ON SCHEMA control TO %s", svc),
	)
	for _, t := range serviceFabric {
		stmts = append(stmts, fmt.Sprintf("GRANT SELECT ON %s TO %s", qualified("public", t), svc))
	}
	for _, t := range serviceRead {
		stmts = append(stmts, fmt.Sprintf("GRANT SELECT ON %s TO %s", qualified("control", t), svc))
	}
	for _, t := range serviceWrite {
		stmts = append(stmts, fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s", qualified("control", t), svc))
	}
	tx, err = conn.Begin(ctx)
	if err != nil {
		return false, passwordsql.Safe(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", rolesLock); err != nil {
		return false, fmt.Errorf("apply control roles: %w", redactErr(err))
	}
	if withPassword {
		protection, err = passwordsql.Protect(ctx, tx)
		if err != nil {
			return false, err
		}
		key, err = passwordsql.NewKey()
		if err != nil {
			return false, err
		}
		if err := passwordsql.Preclaim(ctx, tx, key); err != nil {
			return false, err
		}
	}
	for _, q := range stmts {
		if item, ok := passwordActions[q]; ok {
			if err := passwordsql.Set(ctx, tx, key, item.role, item.password); err != nil {
				return false, err
			}
			continue
		}
		if _, err := tx.Exec(ctx, q); err != nil {
			return false, fmt.Errorf("apply control roles: %w", redactErr(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("apply control roles: commit outcome unconfirmed: %w", redactErr(err))
	}
	committed = true
	after, err := rolesDigest(ctx, conn, r)
	if err != nil {
		return false, errors.New("apply control roles: roles committed; catalog readback failed")
	}
	return before != after, nil
}

// rolesDigest hashes the catalog state the grants and attributes produce (role
// attributes and settings, and the ACLs of the tables, views and columns of
// schemas control and public), so a re-run can say it changed nothing. A
// password change is not visible in it (pg_authid needs a superuser).
func rolesDigest(ctx context.Context, conn *pgx.Conn, r Roles) (string, error) {
	var d string
	err := conn.QueryRow(ctx, `SELECT md5(coalesce(string_agg(x, '|' ORDER BY x), '')) FROM (
		SELECT 'role:' || rolname || ':' || rolsuper || rolcreatedb || rolcreaterole || rolcanlogin || rolinherit || rolreplication || rolbypassrls || coalesce(rolconfig::text, '') AS x
			FROM pg_roles WHERE rolname = ANY($1)
		UNION ALL SELECT 'rel:' || n.nspname || '.' || c.relname || ':' || coalesce(c.relacl::text, '')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('control', 'public') AND c.relkind IN ('r', 'v', 'p')
		UNION ALL SELECT 'col:' || c.relname || '.' || a.attname || ':' || a.attacl::text
			FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname IN ('control', 'public') AND a.attacl IS NOT NULL
		UNION ALL SELECT 'nsp:' || nspname || ':' || coalesce(nspacl::text, '') FROM pg_namespace WHERE nspname IN ('control', 'public')
		UNION ALL SELECT 'db:' || coalesce(datacl::text, '') FROM pg_database WHERE datname = current_database()
		) t`, r.Names()).Scan(&d)
	return d, err
}

// requireQuietLogging is retained for callers of the focused guard control.
func requireQuietLogging(ctx context.Context, tx pgx.Tx) error {
	_, err := passwordsql.Protect(ctx, tx)
	return err
}
