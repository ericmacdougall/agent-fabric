package storetest

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/store"
)

type fixtureDatabaseDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func createFixtureDatabase(ctx context.Context, db fixtureDatabaseDB, name string) (func(context.Context) error, error) {
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		// An occupied PID-derived name or uncertain result confers no ownership.
		return nil, err
	}
	return func(ctx context.Context) error {
		_, err := db.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)")
		return err
	}, nil
}

type fixtureRoles struct {
	roles  store.Roles
	marker string // non-secret per-call identity attached atomically to CREATE ROLE
}

type fixtureRoleDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

func (f fixtureRoles) names() []string {
	return []string{f.roles.App(), f.roles.Runner(), f.roles.Scheduler()}
}

// Serialize claim and reconciliation for this exact fixture prefix. A fresh
// transaction after an uncertain commit waits for the previous claim to end.
func (f fixtureRoles) lock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('fabric.storetest.roles'), pg_catalog.hashtext($1))`, f.roles.Prefix)
	return err
}

func (f fixtureRoles) claim(ctx context.Context, db fixtureRoleDB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // failed or uncertain claim reconciles via the marker
	if err := f.lock(ctx, tx); err != nil {
		return err
	}
	for _, name := range f.names() {
		role := pgx.Identifier{name}.Sanitize()
		if _, err := tx.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
			return err // a collision aborts the entire claim, even on the last role
		}
		// PostgreSQL COMMENT requires a literal. The marker is locally generated
		// lowercase hex with a fixed prefix, never a user value or a password.
		if _, err := tx.Exec(ctx, "COMMENT ON ROLE "+role+" IS '"+f.marker+"'"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (f fixtureRoles) cleanup(ctx context.Context, db fixtureRoleDB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // harmless after a successful commit
	if err := f.lock(ctx, tx); err != nil {
		return err
	}
	// ApplyRoles takes this lock until its server transaction resolves. If its
	// client reports an uncertain failure, wait before examining or dropping
	// the marker set so the still-running writer cannot recreate the roles.
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('fabric.apply_roles'))`); err != nil {
		return err
	}
	names := f.names()
	var owned int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_roles
		WHERE rolname = ANY($1) AND pg_catalog.shobj_description(oid, 'pg_authid') = $2`, names, f.marker).Scan(&owned); err != nil {
		return err
	}
	if owned == 0 {
		return nil // failed claim or already cleaned; never drop by name alone
	}
	if owned != len(names) {
		return errors.New("incomplete fixture role ownership")
	}
	for i := range names {
		names[i] = pgx.Identifier{names[i]}.Sanitize()
	}
	roles := strings.Join(names, ", ")
	if _, err := tx.Exec(ctx, "DROP OWNED BY "+roles); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DROP ROLE "+roles); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
