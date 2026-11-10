// Package control holds the Control Room's storage and projection (goodqa#15586
// F19a). Its schema, `control`, is created by Bootstrap, run once by the
// database owner; it is never a numbered migration and never touches
// fabric_schema, so the schema level and the binary-only rollback window of the
// rest of Fabric are unchanged (spec AC-13).
package control

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var schemaFS embed.FS

// bootstrapLock serialises concurrent bootstraps (a distinct key from the
// outbox order lock).
const bootstrapLock = 7_431_902_003

// ErrDefinitionChanged means the database holds a control schema made from a
// different definition than this binary carries (a changed hash for an applied
// version, a gap in the applied versions, or an applied version this binary
// does not know).
var ErrDefinitionChanged = errors.New("control schema was created from a different definition")

// step is one numbered schema file.
type step struct {
	version int
	sql     string
	sha     string
}

var fileName = regexp.MustCompile(`^(\d{3})_[a-z0-9_]+\.sql$`)

// loadSteps returns the schema files of fsys (directory dir) in version order,
// checking that the numbers are 1..n with no gap.
func loadSteps(fsys fs.FS, dir string) ([]step, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []step
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("control schema file %q is not NNN_name.sql", e.Name())
		}
		n, _ := strconv.Atoi(m[1])
		b, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, step{version: n, sql: string(b), sha: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, st := range out {
		if st.version != i+1 {
			return nil, fmt.Errorf("control schema files have a gap: expected version %d, found %d", i+1, st.version)
		}
	}
	return out, nil
}

func steps() ([]step, error) { return loadSteps(schemaFS, "sql") }

// SchemaVersion is the highest version this binary carries.
func SchemaVersion() int {
	st, err := steps()
	if err != nil {
		return 0
	}
	return len(st)
}

// SchemaSHA256 is the hash of the highest version's definition.
func SchemaSHA256() string {
	st, err := steps()
	if err != nil || len(st) == 0 {
		return ""
	}
	return st[len(st)-1].sha
}

// Bootstrap creates schema control and applies its numbered files in order,
// once each. It is idempotent: an applied version is recognised by its ledger
// row; a ledger row with a different hash, a gap in the applied versions, or an
// applied version this binary does not carry is refused (ErrDefinitionChanged)
// instead of being trusted or altered. It refuses a server_encoding other than
// UTF8 (control.mirror_hash is IMMUTABLE only under it). It runs in one
// transaction under a 5 second lock_timeout and an advisory lock and writes
// only inside schema control: no Fabric table or ledger row changes. The caller
// must connect as the database owner.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	all, err := steps()
	if err != nil {
		return err
	}
	return bootstrapSteps(ctx, pool, all)
}

func bootstrapSteps(ctx context.Context, pool *pgxpool.Pool, all []step) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	var enc string
	if err := tx.QueryRow(ctx, "SHOW server_encoding").Scan(&enc); err != nil {
		return fmt.Errorf("control bootstrap: %w", err)
	}
	if enc != "UTF8" {
		return fmt.Errorf("control bootstrap: server_encoding is %s, UTF8 is required", enc)
	}
	for _, q := range []string{
		"SET LOCAL lock_timeout = '5s'",
		fmt.Sprintf("SELECT pg_advisory_xact_lock(%d)", bootstrapLock),
		"CREATE SCHEMA IF NOT EXISTS control",
		`CREATE TABLE IF NOT EXISTS control.bootstrap_ledger (
			version    integer     PRIMARY KEY,
			sha256     text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("control bootstrap: %w", err)
		}
	}
	rows, err := tx.Query(ctx, "SELECT version, sha256 FROM control.bootstrap_ledger ORDER BY version")
	if err != nil {
		return fmt.Errorf("control bootstrap: %w", err)
	}
	applied := map[int]string{}
	for rows.Next() {
		var v int
		var h string
		if err := rows.Scan(&v, &h); err != nil {
			rows.Close()
			return err
		}
		applied[v] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for v := range applied {
		if v < 1 || v > len(all) {
			return fmt.Errorf("%w: the ledger holds version %d, this binary carries 1 to %d", ErrDefinitionChanged, v, len(all))
		}
	}
	top := 0
	for v := range applied {
		top = max(top, v)
	}
	for v := 1; v <= top; v++ {
		if _, ok := applied[v]; !ok {
			return fmt.Errorf("%w: version %d is applied but version %d is missing from the ledger", ErrDefinitionChanged, top, v)
		}
	}
	missing := false
	for _, st := range all {
		have, ok := applied[st.version]
		switch {
		case ok && have != st.sha:
			return fmt.Errorf("%w: version %d ledger %s, binary %s", ErrDefinitionChanged, st.version, have, st.sha)
		case ok && missing:
			return fmt.Errorf("%w: version %d is applied after a missing earlier version", ErrDefinitionChanged, st.version)
		case !ok:
			missing = true
			if _, err := tx.Exec(ctx, st.sql); err != nil {
				return fmt.Errorf("control bootstrap: version %d: %w", st.version, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO control.bootstrap_ledger (version, sha256) VALUES ($1, $2)", st.version, st.sha); err != nil {
				return fmt.Errorf("control bootstrap: %w", err)
			}
		}
	}
	return tx.Commit(ctx)
}
