package control

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// checkable is the allowlist of other databases VerifyRoles may connect to
// (default deny): fabric_live, rehearsal copies and scratch databases. Every other
// database on the shared cluster (other products, among them the ones the user
// has ruled out) is recorded as unchecked and never connected to. The CONNECT
// right of the control roles on each of them is still read from the shared
// catalog, which needs no connection.
var checkable = regexp.MustCompile(`^(fabric_live|fabric_r15299_[a-z0-9_]+|fabtest_[0-9]+)$`)

// otherDatabases implements the code part of spec AC-14 (goodqa#15623): for each
// allowlisted other database it connects read-only and checks that no control role
// may CREATE in schema public or in the database; for the rest it only reports
// that they were not checked. It returns problems (real findings) and notes.
func otherDatabases(ctx context.Context, pool *pgxpool.Pool, r Roles, current string) (problems, notes []string, err error) {
	rows, err := pool.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname <> $1 ORDER BY datname`, current)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var unchecked []string
	for _, n := range names {
		if !checkable.MatchString(n) {
			unchecked = append(unchecked, n)
			continue
		}
		ps, note, err := checkOneDatabase(ctx, pool, r, n)
		if err != nil {
			notes = append(notes, fmt.Sprintf("other_db_rights: database %s: unchecked, cannot connect read-only (%s)", n, redactErr(err)))
			continue
		}
		problems = append(problems, ps...)
		notes = append(notes, note)
	}
	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		shown := unchecked
		if len(shown) > 8 {
			shown = shown[:8]
		}
		var connect int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_database d, unnest($2::text[]) AS role WHERE d.datname = ANY($1) AND has_database_privilege(role, d.datname, 'CONNECT')`, unchecked, r.Names()).Scan(&connect); err != nil {
			return nil, nil, err
		}
		notes = append(notes, fmt.Sprintf("other_db_rights: %d other databases unchecked, not connected (default deny; first %d: %s); the control roles hold CONNECT on %d of the %d role-database pairs (PUBLIC's default, disclosed in AC-14)",
			len(unchecked), len(shown), strings.Join(shown, ", "), connect, len(unchecked)*len(r.Names())))
	}
	return problems, notes, nil
}

// checkOneDatabase connects to one allowlisted database with the pool's
// credentials (read-only transaction) and checks the CREATE rights of the roles.
func checkOneDatabase(ctx context.Context, pool *pgxpool.Pool, r Roles, name string) ([]string, string, error) {
	cfg := pool.Config().ConnConfig.Copy()
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	var problems []string
	for _, role := range r.Names() {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			return nil, "", err
		}
		if !exists {
			continue
		}
		var schemaCreate, dbCreate bool
		if err := conn.QueryRow(ctx, "SELECT has_schema_privilege($1, 'public', 'CREATE'), has_database_privilege($1, current_database(), 'CREATE')", role).Scan(&schemaCreate, &dbCreate); err != nil {
			return nil, "", err
		}
		if schemaCreate {
			problems = append(problems, fmt.Sprintf("role %s may CREATE in schema public of database %s", role, name))
		}
		if dbCreate {
			problems = append(problems, fmt.Sprintf("role %s may CREATE in database %s", role, name))
		}
	}
	return problems, fmt.Sprintf("other_db_rights: database %s: checked (schema public and database CREATE), %d problem(s)", name, len(problems)), nil
}
