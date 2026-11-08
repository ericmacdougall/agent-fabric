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

// ChildDSNEnv names the variable a test sets for a child process it starts
// (a re-run of its own test binary), so the child uses the parent's
// database instead of creating its own. A child must not call Open, which
// empties the database.
const ChildDSNEnv = "FABRIC_STORETEST_CHILD_DSN"

// Main creates and migrates fabric_<prefix>_<pid>, runs the tests and drops
// it. Call it as os.Exit(storetest.Main(m, "imp")) from TestMain.
func Main(m *testing.M, prefix string) int {
	if child := os.Getenv(ChildDSNEnv); child != "" {
		DSN = child // a test's child process: the parent owns the database
		return m.Run()
	}
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

// Open returns a store on an emptied test database, closed at the end of the
// test. Emptying makes each test (and each -count iteration) independent.
func Open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Truncate(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

// RunnerDSN gives the test database least-privilege roles (goodqa#14929)
// and returns a DSN whose sessions run as the runner role. The roles are
// cluster-wide, so they carry this process's id and are dropped at the end
// of the test.
func RunnerDSN(t *testing.T, st *store.Store) string {
	t.Helper()
	r := store.Roles{Prefix: fmt.Sprintf("fabric_s%d", os.Getpid())}
	if err := st.ApplyRoles(context.Background(), r, "", ""); err != nil {
		t.Fatalf("roles: %v", err)
	}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			st.Pool().Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			st.Pool().Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	u, err := url.Parse(DSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("role", r.Runner())
	u.RawQuery = q.Encode()
	return u.String()
}
