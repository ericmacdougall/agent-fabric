package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
func redactErr(err error) error {
	var pe interface{ SQLState() string }
	if errors.As(err, &pe) {
		return fmt.Errorf("sqlstate %s", pe.SQLState())
	}
	return errors.New("statement failed")
}

// ApplyRoles creates or updates the three control roles and their grants. It
// is idempotent, runs as the database owner in one transaction under an
// advisory lock, touches no fabric_* role, no fabric_settings row and no
// password of any other role, and never logs a password. The control schema must
// already exist (fab control bootstrap).
func ApplyRoles(ctx context.Context, pool *pgxpool.Pool, r Roles, pw Passwords) (changed bool, err error) {
	if !rolePrefix.MatchString(r.Prefix) {
		return false, fmt.Errorf("role prefix %q", r.Prefix)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	// On uncertain transaction completion or failed restoration checks, discard
	// this physical connection rather than letting protected session state escape.
	discard := false
	defer func() {
		if discard {
			conn.Hijack().Close(ctx) //nolint:errcheck // Close destroys even with an expired context
		} else {
			conn.Release()
		}
	}()
	withPassword := pw.Export != "" || pw.Ingest != "" || pw.Service != ""
	var activityBefore string
	if withPassword {
		if err := conn.QueryRow(ctx, "SELECT current_setting('track_activities')").Scan(&activityBefore); err != nil {
			return false, errors.New("apply control roles: cannot inspect activity tracking; no roles changed")
		}
	}
	quote := func(v string) (string, error) {
		q, err := conn.Conn().PgConn().EscapeString(v)
		return "'" + q + "'", err
	}
	login := func(p string) (string, error) {
		if p == "" {
			return "NOLOGIN PASSWORD NULL", nil
		}
		q, err := quote(p)
		return "LOGIN PASSWORD " + q, err
	}
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return false, err
	}
	var haveControl bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('control.ingested_events') IS NOT NULL").Scan(&haveControl); err != nil {
		return false, err
	}
	if !haveControl {
		return false, errors.New("the control schema does not exist: run fab control bootstrap first")
	}
	var stmts []string
	before, err := rolesDigest(ctx, conn.Conn(), r)
	if err != nil {
		return false, err
	}
	for _, c := range []struct{ name, pw string }{{r.Export(), pw.Export}, {r.Ingest(), pw.Ingest}, {r.Service(), pw.Service}} {
		opts, err := login(c.pw)
		if err != nil {
			return false, redactErr(err)
		}
		n := quoteID(c.name)
		stmts = append(stmts,
			fmt.Sprintf(`DO $do$ BEGIN CREATE ROLE %s NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $do$`, n),
			fmt.Sprintf("ALTER ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT %s", n, opts),
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
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				discard = true
			}
		}
		if conn.Conn().PgConn().TxStatus() != 'I' {
			discard = true
		}
		if !discard && withPassword {
			var restored string
			if checkErr := conn.QueryRow(ctx, "SELECT current_setting('track_activities')").Scan(&restored); checkErr != nil || restored != activityBefore {
				discard = true
				if err == nil {
					if committed {
						err = errors.New("apply control roles: roles committed; session restoration could not be verified; connection discarded")
					} else {
						err = errors.New("apply control roles: session restoration could not be verified; connection discarded")
					}
				}
			}
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", rolesLock); err != nil {
		return false, fmt.Errorf("apply control roles: %w", redactErr(err))
	}
	if withPassword {
		if err := requireQuietLogging(ctx, tx); err != nil {
			return false, err
		}
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return false, fmt.Errorf("apply control roles: %w", redactErr(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		discard = true
		return false, fmt.Errorf("apply control roles: commit outcome unconfirmed: %w", redactErr(err))
	}
	committed = true
	after, err := rolesDigest(ctx, conn.Conn(), r)
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

// requireQuietLogging keeps a password literal out of the server log and out of
// pg_stat_statements until the SCRAM verifier change (goodqa#15594) removes
// plaintext: a failing ALTER ROLE ... PASSWORD is logged with its statement text
// when log_min_error_statement is error or lower. A superuser sets the logging
// settings for this transaction; any other caller must already run with
// log_min_error_statement=panic, log_statement=none and
// log_min_duration_statement=-1 (for example through PGOPTIONS). When pg_stat_statements
// tracks utility statements it must be turned off for the transaction, or the run
// is refused. A refusal sends no password.
func requireQuietLogging(ctx context.Context, tx pgx.Tx) error {
	// Protect the outer transaction before any role mutation or plaintext SQL.
	// Do not put this in a savepoint that could roll back the protection.
	if _, err := tx.Exec(ctx, "SET LOCAL track_activities = off"); err != nil {
		return errors.New("apply control roles: refusing to send a password: cannot disable activity tracking for this transaction")
	}
	var tracking string
	if err := tx.QueryRow(ctx, "SELECT current_setting('track_activities')").Scan(&tracking); err != nil || tracking != "off" {
		return errors.New("apply control roles: refusing to send a password: activity tracking protection cannot be verified")
	}
	var super bool
	if err := tx.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		return err
	}
	if super {
		for _, q := range []string{
			"SET LOCAL log_min_error_statement = panic",
			"SET LOCAL log_statement = none",
			"SET LOCAL log_min_duration_statement = -1",
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return errors.New("apply control roles: cannot set the logging parameters for this transaction")
			}
		}
	}
	var lmes, ls, lmd string
	if err := tx.QueryRow(ctx, "SELECT current_setting('log_min_error_statement'), current_setting('log_statement'), current_setting('log_min_duration_statement')").Scan(&lmes, &ls, &lmd); err != nil {
		return err
	}
	if lmes != "panic" || ls != "none" || lmd != "-1" {
		return fmt.Errorf("apply control roles: refusing to send a password: log_min_error_statement=%s log_statement=%s log_min_duration_statement=%s would let the server log a failing ALTER ROLE statement with the password in it; connect with PGOPTIONS='-c log_min_error_statement=panic -c log_statement=none -c log_min_duration_statement=-1' (or wait for the SCRAM verifier change goodqa#15594)", lmes, ls, lmd)
	}
	var v *string
	if err := tx.QueryRow(ctx, "SELECT current_setting('pg_stat_statements.track_utility', true)").Scan(&v); err != nil {
		return err
	}
	if v == nil || *v == "off" {
		return nil
	}
	if _, err := tx.Exec(ctx, "SET LOCAL pg_stat_statements.track_utility = off"); err != nil {
		return errors.New("apply control roles: pg_stat_statements tracks utility statements and this session cannot turn that off; refusing to send a password (set pg_stat_statements.track_utility = off, or wait for the SCRAM verifier change goodqa#15594)")
	}
	if err := tx.QueryRow(ctx, "SELECT current_setting('pg_stat_statements.track_utility')").Scan(&tracking); err != nil || tracking != "off" {
		return errors.New("apply control roles: refusing to send a password: utility tracking protection cannot be verified")
	}
	return nil
}
