//go:build linux

package connref

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDisposableFixtureExactMatchAndClose(t *testing.T) {
	const password = "disposable:fixture\\only"
	f, err := NewFixture("postgres://admin@127.0.0.1:5432/fabric_fixture?sslmode=disable", Runner, "fixture_runner", password, privateFixtureDirectory(t))
	if err != nil {
		t.Fatal("create disposable fixture:", err)
	}
	defer f.Close()
	dsn, err := f.DSN()
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("fixture URL invalid")
	}
	if _, present := u.User.Password(); present || strings.Contains(dsn, password) {
		t.Fatal("fixture URL contains a password")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil || cfg.User != "fixture_runner" || cfg.Password != password || cfg.Database != "fabric_fixture" {
		t.Fatal("fixture did not load exact connection credential")
	}
	path := u.Query().Get("passfile")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("fixture passfile is not private")
	}
	u.Path = "/another_database"
	other, err := pgconn.ParseConfig(u.String())
	if err != nil || other.Password != "" {
		t.Fatal("fixture credential matched another database")
	}
	if err := f.Close(); err != nil {
		t.Fatal("fixture close failed")
	}
	if err := f.Close(); err != nil {
		t.Fatal("fixture close was not idempotent")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("fixture file survived Close")
	}
	if _, err := f.DSN(); err == nil {
		t.Fatal("closed fixture remained usable")
	}
}

func TestDisposableFixtureRejectsOverridesWithoutEcho(t *testing.T) {
	for _, base := range []string{
		"postgres://admin@127.0.0.1:5432/*?sslmode=disable",
		"postgres://admin@127.0.0.1:5432/fabric_fixture?sslmode=disable&sslpassword=forbidden",
		"postgres://admin@127.0.0.1:5432/fabric_fixture?sslmode=disable&unknown=forbidden",
		"postgres://admin@127.0.0.1:5432/fabric_fixture?sslmode=disable&sslmode=require",
	} {
		f, err := NewFixture(base, Runner, "fixture_runner", "synthetic", privateFixtureDirectory(t))
		if f != nil {
			f.Close()
		}
		if err == nil {
			t.Fatal("unsafe fixture base accepted")
		}
		if strings.Contains(err.Error(), "forbidden") {
			t.Fatal("fixture error echoed input")
		}
	}
}

func TestDisposableFixtureFileErrorCannotFallBack(t *testing.T) {
	t.Setenv("FABRIC_TEST_DSN", "postgres://legacy@127.0.0.1/postgres?sslmode=disable")
	t.Setenv("FABRIC_TEST_DSN_FILE", "/missing/synthetic-source")
	ref, err := ResolveEnv("FABRIC_TEST_DSN", TestAdmin)
	if ref != nil {
		ref.Close()
	}
	if err == nil {
		t.Fatal("invalid file configuration fell back to raw environment")
	}
}

func privateFixtureDirectory(t *testing.T) string {
	t.Helper()
	dir, err := NewFixtureDirectory(t.TempDir())
	if err != nil {
		t.Fatal("create private fixture directory failed")
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("new fixture directory is not private")
	}
	return dir
}
