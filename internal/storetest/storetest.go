// Package storetest gives a package's tests a disposable, migrated Postgres
// database. It fails loudly without FABRIC_TEST_DSN: a green run that never
// touched the database would be vacuous.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/connref"
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
func Main(m *testing.M, prefix string) (code int) {
	if child := os.Getenv(ChildDSNEnv); child != "" {
		DSN = child // the parent retains its reference until this child exits
		return m.Run()
	}
	adminRef, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test admin credential reference unavailable")
		return 2
	}
	defer adminRef.Close()
	admin, err := adminRef.DSN()
	if err != nil || admin == "" {
		fmt.Fprintln(os.Stderr, "FABRIC_TEST_DSN or FABRIC_TEST_DSN_FILE is required: these tests need a disposable Postgres")
		return 2
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test admin connection failed")
		return 2
	}
	defer pool.Close()
	name := fmt.Sprintf("fabric_%s_%d", prefix, os.Getpid())
	pool.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`) //nolint:errcheck // owned leftover from a killed run
	if _, err := pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		fmt.Fprintln(os.Stderr, "create test database failed")
		return 2
	}
	var dbRef *connref.Ref
	defer func() {
		if _, err := pool.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			fmt.Fprintln(os.Stderr, "drop test database failed")
			code = 2
		}
		if dbRef != nil {
			dbRef.Close()
		}
	}()
	dbRef, err = connref.ForTestDatabase(adminRef, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "derive test database credential reference failed")
		return 2
	}
	DSN, err = dbRef.DSN()
	if err != nil {
		return 2
	}
	st, err := store.Open(ctx, DSN)
	if err == nil {
		err = st.Migrate(ctx)
		st.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate test database failed")
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
		t.Fatal("open test database failed")
	}
	t.Cleanup(st.Close)
	if err := st.Truncate(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

// AppRunnerDSNs gives the test database least-privilege roles and returns
// directly authenticated app and runner DSNs with distinct test-only passwords.
// The roles are cluster-wide, so they carry this process's id and are dropped
// at the end of the test.
func AppRunnerDSNs(t *testing.T, st *store.Store) (string, string) {
	t.Helper()
	r := store.Roles{Prefix: fmt.Sprintf("fabric_s%d", os.Getpid())}
	// Random per call: these are cluster-wide LOGIN roles and the test cluster
	// may host other databases, so no fixed or guessable password is used.
	randomPassword := func() string {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	appPassword, runnerPassword := randomPassword(), randomPassword()
	if err := st.ApplyRoles(context.Background(), r, appPassword, runnerPassword); err != nil {
		t.Fatal("apply test roles failed")
	}
	fixtureDir, err := connref.NewFixtureDirectory(t.TempDir())
	if err != nil {
		t.Fatal("create private credential fixture directory failed")
	}
	appRef, err := connref.NewFixture(DSN, connref.App, r.App(), appPassword, fixtureDir)
	if err != nil {
		t.Fatal("create app credential fixture failed")
	}
	t.Cleanup(func() { appRef.Close() })
	runnerRef, err := connref.NewFixture(DSN, connref.Runner, r.Runner(), runnerPassword, fixtureDir)
	if err != nil {
		t.Fatal("create runner credential fixture failed")
	}
	t.Cleanup(func() { runnerRef.Close() })
	// Cleanup is LIFO: roles still have their authenticated stores/fixtures;
	// then fixtures close, then the private directory and caller's store.
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			st.Pool().Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			st.Pool().Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	app, err := appRef.DSN()
	if err != nil {
		t.Fatal("app credential fixture unavailable")
	}
	runner, err := runnerRef.DSN()
	if err != nil {
		t.Fatal("runner credential fixture unavailable")
	}
	return app, runner
}

// RunnerDSN retains the original runner-only test helper.
func RunnerDSN(t *testing.T, st *store.Store) string {
	t.Helper()
	_, runner := AppRunnerDSNs(t, st)
	return runner
}
