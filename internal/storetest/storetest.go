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
	"sync"
	"testing"
	"time"

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
	drop, err := createFixtureDatabase(ctx, pool, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test database failed")
		return 2
	}
	var dbRef *connref.Ref
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := drop(ctx); err != nil {
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

// fixtureFactories is per call so an isolated regression can exercise real
// filesystem failures after provisioning without global mutable hooks.
type fixtureFactories struct {
	directory  func(string) (string, error)
	credential func(string, connref.Purpose, string, string, string) (*connref.Fixture, error)
	applyRoles func(context.Context, store.Roles, string, string) error
}

// AppRunnerDSNs gives the test database least-privilege roles and returns
// directly authenticated app and runner DSNs with distinct test-only passwords.
// The roles are cluster-wide, so they carry this process's id and are dropped
// at the end of the test.
func AppRunnerDSNs(t *testing.T, st *store.Store) (string, string) {
	t.Helper()
	// One claim per test: a second call in the same test (a setup helper that
	// calls it after another helper did) returns the same borrowed DSNs instead of
	// colliding with the roles the first call claimed. They stay valid until the
	// test's cleanup, which runs after the test's own connection cleanup.
	pairsMu.Lock()
	if p, ok := pairs[t]; ok {
		pairsMu.Unlock()
		return p[0], p[1]
	}
	pairsMu.Unlock()
	app, runner := appRunnerDSNs(t, st, fixtureFactories{directory: connref.NewFixtureDirectory, credential: connref.NewFixture})
	pairsMu.Lock()
	pairs[t] = [2]string{app, runner}
	pairsMu.Unlock()
	t.Cleanup(func() {
		pairsMu.Lock()
		delete(pairs, t)
		pairsMu.Unlock()
	})
	return app, runner
}

var (
	pairsMu sync.Mutex
	pairs   = map[*testing.T][2]string{}
)

func appRunnerDSNs(t *testing.T, st *store.Store, factories fixtureFactories) (string, string) {
	t.Helper()
	// Register the parent directory's cleanup before role provisioning, so it
	// cannot erase partial fixtures before the coordinator below closes them.
	fixtureParent := t.TempDir()
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
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate test role ownership marker failed")
	}
	owned := fixtureRoles{roles: r, marker: "fabric-storetest:" + hex.EncodeToString(nonce[:])}
	var fixtureDir string
	var appRef, runnerRef *connref.Fixture
	// Register before CREATE. A lost commit acknowledgement or an ApplyRoles
	// error after commit still leaves the atomic creation marker for cleanup.
	t.Cleanup(func() {
		// Callers register connection/pool cleanup after this helper returns,
		// so LIFO closes those users first. These refs live through role cleanup.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := owned.cleanup(ctx, st.Pool()); err != nil {
			t.Error("cleanup test role ownership failed")
		}
		if runnerRef != nil {
			if err := runnerRef.Close(); err != nil {
				t.Error("cleanup runner credential fixture failed")
			}
		}
		if appRef != nil {
			if err := appRef.Close(); err != nil {
				t.Error("cleanup app credential fixture failed")
			}
		}
		if fixtureDir != "" {
			if err := os.Remove(fixtureDir); err != nil && !os.IsNotExist(err) {
				t.Error("cleanup credential fixture directory failed")
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := owned.claim(ctx, st.Pool()); err != nil {
		t.Fatal("claim test roles failed")
	}
	applyRoles := factories.applyRoles
	if applyRoles == nil {
		applyRoles = st.ApplyRoles
	}
	if err := applyRoles(ctx, r, appPassword, runnerPassword); err != nil {
		t.Fatal("apply test roles failed")
	}
	fixtureDir, err := factories.directory(fixtureParent)
	if err != nil {
		t.Fatal("create private credential fixture directory failed")
	}
	appRef, err = factories.credential(DSN, connref.App, r.App(), appPassword, fixtureDir)
	if err != nil {
		t.Fatal("create app credential fixture failed")
	}
	runnerRef, err = factories.credential(DSN, connref.Runner, r.Runner(), runnerPassword, fixtureDir)
	if err != nil {
		t.Fatal("create runner credential fixture failed")
	}
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
