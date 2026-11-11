package storetest

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/store"
)

type existingDatabase struct{ commands []string }

func (d *existingDatabase) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	d.commands = append(d.commands, sql)
	return pgconn.CommandTag{}, errors.New("duplicate database")
}

func TestFixtureDatabaseCollisionCannotAuthorizeDrop(t *testing.T) {
	db := &existingDatabase{}
	drop, err := createFixtureDatabase(context.Background(), db, "fabric_fixture_cleanup_123")
	if err == nil || drop != nil || len(db.commands) != 1 || !strings.HasPrefix(db.commands[0], "CREATE DATABASE ") {
		t.Fatal("collision must issue only CREATE and convey no cleanup authority")
	}
}

type roleCatalog struct {
	markers map[string]string
	loseAck bool
}

func (c *roleCatalog) Begin(context.Context) (pgx.Tx, error) {
	return &catalogTx{catalog: c, pending: maps.Clone(c.markers)}, nil
}

type catalogTx struct {
	pgx.Tx
	catalog *roleCatalog
	pending map[string]string
}

func (tx *catalogTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	words := strings.Fields(sql)
	switch {
	case strings.HasPrefix(sql, "SELECT pg_catalog.pg_advisory_xact_lock("):
	case strings.HasPrefix(sql, "CREATE ROLE "):
		name := strings.Trim(words[2], `"`)
		if _, exists := tx.pending[name]; exists {
			return pgconn.CommandTag{}, errors.New("duplicate role")
		}
		tx.pending[name] = ""
	case strings.HasPrefix(sql, "COMMENT ON ROLE "):
		tx.pending[strings.Trim(words[3], `"`)] = strings.Trim(words[5], "'")
	case strings.HasPrefix(sql, "DROP OWNED BY "):
	case strings.HasPrefix(sql, "DROP ROLE "):
		for _, name := range words[2:] {
			delete(tx.pending, strings.Trim(name, `",`))
		}
	default:
		return pgconn.CommandTag{}, errors.New("unexpected SQL")
	}
	return pgconn.CommandTag{}, nil
}

func (tx *catalogTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "shobj_description") {
		return roleCount(-1)
	}
	var count roleCount
	for _, name := range args[0].([]string) {
		if tx.pending[name] == args[1].(string) {
			count++
		}
	}
	return count
}

func (tx *catalogTx) Commit(context.Context) error {
	tx.catalog.markers = tx.pending
	if tx.catalog.loseAck {
		tx.catalog.loseAck = false
		return errors.New("synthetic lost commit acknowledgement")
	}
	return nil
}

func (tx *catalogTx) Rollback(context.Context) error { return nil }

type roleCount int

func (n roleCount) Scan(dest ...any) error {
	if n < 0 {
		return errors.New("missing ownership predicate")
	}
	*dest[0].(*int) = int(n)
	return nil
}

func TestFixtureRoleClaimCollisionLeavesExistingRoleUntouched(t *testing.T) {
	ctx := context.Background()
	owner := fixtureRoles{store.Roles{Prefix: "fabric_s123"}, "fabric-storetest:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	// Collision on the last role means the first two attempted creates must
	// roll back. The subsequent cleanup must preserve the existing object.
	want := map[string]string{owner.roles.Scheduler(): "other-owner"}
	db := &roleCatalog{markers: maps.Clone(want)}
	if err := owner.claim(ctx, db); err == nil {
		t.Fatal("accepted preexisting scheduler role")
	}
	if err := owner.cleanup(ctx, db); err != nil || !maps.Equal(db.markers, want) {
		t.Fatal("collision cleanup changed roles")
	}
}

func TestFixtureRoleUncertainCommitReconcilesOnlyOwnedMarkers(t *testing.T) {
	ctx := context.Background()
	owner := fixtureRoles{store.Roles{Prefix: "fabric_s123"}, "fabric-storetest:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	want := map[string]string{"other_role": "other-owner"}
	db := &roleCatalog{markers: maps.Clone(want), loseAck: true}
	if err := owner.claim(ctx, db); err == nil || len(db.markers) != 4 {
		t.Fatal("fixture did not observe a committed claim with lost acknowledgement")
	}
	db.markers[owner.roles.App()] = "replacement-owner"
	if err := owner.cleanup(ctx, db); err == nil || len(db.markers) != 4 {
		t.Fatal("partial ownership must preserve every role and refuse")
	}
	db.markers[owner.roles.App()] = owner.marker
	if err := owner.cleanup(ctx, db); err != nil || !maps.Equal(db.markers, want) {
		t.Fatal("owned claim did not reconcile cleanly")
	}
}
