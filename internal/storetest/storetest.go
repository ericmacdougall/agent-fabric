// Package storetest gives a package's tests a disposable, migrated Postgres
// database. It fails loudly without FABRIC_TEST_DSN: a green run that never
// touched the database would be vacuous.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/store"
)

// DSN is the per-process test database, valid inside m.Run.
var DSN string

// Main creates and migrates fabric_<prefix>_<pid>, runs the tests and drops
// it. Call it as os.Exit(storetest.Main(m, "imp")) from TestMain.
func Main(m *testing.M, prefix string) int {
	admin := os.Getenv("FABRIC_TEST_DSN")
	if admin == "" {
		fmt.Fprintln(os.Stderr, "FABRIC_TEST_DSN is not set: these tests require a disposable Postgres")
		return 2
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin connect:", err)
		return 2
	}
	defer pool.Close()
	name := fmt.Sprintf("fabric_%s_%d", prefix, os.Getpid())
	pool.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`) //nolint:errcheck // leftover from a killed run
	if _, err := pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		fmt.Fprintln(os.Stderr, "create database:", err)
		return 2
	}
	defer pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`) //nolint:errcheck
	u, err := url.Parse(admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsn does not parse")
		return 2
	}
	u.Path = "/" + name
	DSN = u.String()
	st, err := store.Open(ctx, DSN)
	if err == nil {
		err = st.Migrate(ctx)
		st.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 2
	}
	return m.Run()
}

// Open returns a store on the test database, closed at the end of the test.
func Open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}
