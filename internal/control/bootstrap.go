// Package control holds the Control Room's storage and projection (goodqa#15586
// F19a). Its schema, `control`, is created by Bootstrap, run once by the
// database owner; it is never a numbered migration and never touches
// fabric_schema, so the schema level and the binary-only rollback window of the
// rest of Fabric are unchanged (spec AC-13).
package control

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/001_schema.sql
var schemaSQL string

// SchemaVersion is the version of the definition in sql/001_schema.sql.
const SchemaVersion = 1

// bootstrapLock serialises concurrent bootstraps (a distinct key from the
// outbox order lock).
const bootstrapLock = 7_431_902_003

// ErrDefinitionChanged means the database holds a control schema made from a
// different definition than this binary carries.
var ErrDefinitionChanged = errors.New("control schema was created from a different definition")

// SchemaSHA256 is the hash of the embedded definition, recorded in the ledger.
func SchemaSHA256() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}

// Bootstrap creates schema control and its tables, once. It is idempotent: a
// second call finds its ledger row and does nothing, and a ledger row with a
// different hash is refused (ErrDefinitionChanged) instead of being trusted.
// It runs in one transaction under a 5 second lock_timeout and an advisory lock
// and writes only inside schema control: no Fabric table or ledger row changes.
// The caller must connect as the database owner.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
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
	var have string
	err = tx.QueryRow(ctx, "SELECT sha256 FROM control.bootstrap_ledger WHERE version = $1", SchemaVersion).Scan(&have)
	switch {
	case err == nil:
		if have != SchemaSHA256() {
			return fmt.Errorf("%w: ledger %s, binary %s", ErrDefinitionChanged, have, SchemaSHA256())
		}
		return tx.Commit(ctx) // already bootstrapped from this definition
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("control bootstrap: %w", err)
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("control bootstrap: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO control.bootstrap_ledger (version, sha256) VALUES ($1, $2)", SchemaVersion, SchemaSHA256()); err != nil {
		return fmt.Errorf("control bootstrap: %w", err)
	}
	return tx.Commit(ctx)
}
