package control

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// roles-matrix.json is a hand-written literal of what each control role may do
// (goodqa#15623 AC-1 to AC-3). It is not derived from the grant code in roles.go:
// a wrong grant cannot also change this file. VerifyRoles and the tests compare
// the live catalog with it; anything not listed is expected to be denied.
//
//go:embed roles-matrix.json
var matrixJSON []byte

type matrixFile struct {
	Tables        map[string]map[string]string
	InsertColumns map[string]map[string][]string `json:"insert_columns"`
	SchemaUsage   map[string][]string            `json:"schema_usage"`
	Database      struct{ Connect, Create, Temp bool }
	// Functions in schema public: EXECUTE is expected on every function except a
	// SECURITY DEFINER one and the ones with an explicit revoke, plus the literal
	// exceptions below (the known existing state of Fabric, goodqa#15628).
	DefinerPublicExecute []string            `json:"definer_public_execute"`
	NondefinerDenied     []string            `json:"nondefiner_denied"`
	ControlExecute       map[string][]string `json:"control_function_execute"`
}

func loadMatrix() (matrixFile, error) {
	var m matrixFile
	err := json.Unmarshal(matrixJSON, &m)
	return m, err
}

func privHas(privs, want string) bool {
	for _, p := range strings.Split(privs, ",") {
		if p == want {
			return true
		}
	}
	return false
}

func inList(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// matrixProblems compares one role (kind export, ingest or service) with the
// matrix, cell by cell: every table, view and column privilege in schemas public
// and control, sequences, functions, schema USAGE and CREATE, the database
// privileges, and default privileges. It only reads the catalog.
func matrixProblems(ctx context.Context, pool *pgxpool.Pool, kind, role string) ([]string, error) {
	m, err := loadMatrix()
	if err != nil {
		return nil, err
	}
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	type rel struct{ schema, name, oid string }
	relsOf := func(kinds string) ([]rel, error) {
		rows, err := pool.Query(ctx, `SELECT n.nspname, c.relname, c.oid::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname IN ('public', 'control') AND c.relkind = ANY(string_to_array($1, ','))`, kinds)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var l []rel
		for rows.Next() {
			var r rel
			if err := rows.Scan(&r.schema, &r.name, &r.oid); err != nil {
				return nil, err
			}
			l = append(l, r)
		}
		return l, rows.Err()
	}
	tables, err := relsOf("r,v,p")
	if err != nil {
		return nil, err
	}
	for _, r := range tables {
		full := r.schema + "." + r.name
		want := m.Tables[kind][full]
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			var got bool
			if err := pool.QueryRow(ctx, "SELECT has_table_privilege($1, $2::oid, $3)", role, r.oid, priv).Scan(&got); err != nil {
				return nil, err
			}
			if got != privHas(want, priv) {
				add("%s %s on %s: got %t, want %t", kind, priv, full, got, privHas(want, priv))
			}
		}
		ins := map[string]bool{}
		for _, c := range m.InsertColumns[kind][full] {
			ins[c] = true
		}
		crows, err := pool.Query(ctx, `SELECT attnum, attname FROM pg_attribute WHERE attrelid = $1::oid AND attnum > 0 AND NOT attisdropped`, r.oid)
		if err != nil {
			return nil, err
		}
		type col struct {
			n    int
			name string
		}
		var cols []col
		for crows.Next() {
			var c col
			if err := crows.Scan(&c.n, &c.name); err != nil {
				crows.Close()
				return nil, err
			}
			cols = append(cols, c)
		}
		crows.Close()
		for _, c := range cols {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"} {
				var got bool
				if err := pool.QueryRow(ctx, "SELECT has_column_privilege($1, $2::oid, $3::smallint, $4)", role, r.oid, c.n, priv).Scan(&got); err != nil {
					return nil, err
				}
				w := privHas(want, priv) || (priv == "INSERT" && ins[c.name])
				if got != w {
					add("%s %s on column %s.%s: got %t, want %t", kind, priv, full, c.name, got, w)
				}
			}
		}
	}
	seqs, err := relsOf("S")
	if err != nil {
		return nil, err
	}
	for _, r := range seqs {
		for _, priv := range []string{"USAGE", "SELECT", "UPDATE"} {
			var got bool
			if err := pool.QueryRow(ctx, "SELECT has_sequence_privilege($1, $2::oid, $3)", role, r.oid, priv).Scan(&got); err != nil {
				return nil, err
			}
			if got {
				add("%s has %s on sequence %s.%s, want none", kind, priv, r.schema, r.name)
			}
		}
	}
	frows, err := pool.Query(ctx, `SELECT n.nspname, p.proname, p.oid::text, p.prosecdef FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('public', 'control') AND p.prokind = 'f'`)
	if err != nil {
		return nil, err
	}
	type fn struct {
		schema, name, oid string
		definer           bool
	}
	var fns []fn
	for frows.Next() {
		var f fn
		if err := frows.Scan(&f.schema, &f.name, &f.oid, &f.definer); err != nil {
			frows.Close()
			return nil, err
		}
		fns = append(fns, f)
	}
	frows.Close()
	for _, f := range fns {
		var got bool
		if err := pool.QueryRow(ctx, "SELECT has_function_privilege($1, $2::oid, 'EXECUTE')", role, f.oid).Scan(&got); err != nil {
			return nil, err
		}
		want := f.schema == "public" && !f.definer && !inList(m.NondefinerDenied, f.name)
		if f.schema == "public" && f.definer && inList(m.DefinerPublicExecute, f.name) {
			want = true
		}
		if f.schema == "control" && inList(m.ControlExecute[kind], f.name) {
			want = true
		}
		if got != want {
			add("%s EXECUTE on %s.%s (security definer %t): got %t, want %t", kind, f.schema, f.name, f.definer, got, want)
		}
	}
	usage := map[string]bool{}
	for _, s := range m.SchemaUsage[kind] {
		usage[s] = true
	}
	for _, s := range []string{"public", "control"} {
		var u, c bool
		if err := pool.QueryRow(ctx, "SELECT has_schema_privilege($1, $2, 'USAGE'), has_schema_privilege($1, $2, 'CREATE')", role, s).Scan(&u, &c); err != nil {
			return nil, err
		}
		if u != usage[s] || c {
			add("%s schema %s: USAGE %t (want %t), CREATE %t (want false)", kind, s, u, usage[s], c)
		}
	}
	var conn, create, temp bool
	if err := pool.QueryRow(ctx, "SELECT has_database_privilege($1, current_database(), 'CONNECT'), has_database_privilege($1, current_database(), 'CREATE'), has_database_privilege($1, current_database(), 'TEMP')", role).Scan(&conn, &create, &temp); err != nil {
		return nil, err
	}
	if conn != m.Database.Connect || create != m.Database.Create || temp != m.Database.Temp {
		add("%s database: CONNECT %t CREATE %t TEMP %t, want %t %t %t", kind, conn, create, temp, m.Database.Connect, m.Database.Create, m.Database.Temp)
	}
	var defaults int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_default_acl d WHERE d.defaclnamespace = to_regnamespace('control')
		OR d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = $1)
		OR EXISTS (SELECT 1 FROM aclexplode(d.defaclacl) a WHERE a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1))`, role).Scan(&defaults); err != nil {
		return nil, err
	}
	if defaults > 0 {
		add("%s: %d default privilege entries mention the role or schema control, want 0", kind, defaults)
	}
	return out, nil
}
