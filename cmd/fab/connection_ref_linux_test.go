//go:build linux

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func syntheticConnectionFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "credential-source")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal("write synthetic connection source")
	}
	return p
}

func TestAppConnectionUsesProtectedFile(t *testing.T) {
	const marker = "synthetic-connection-fixture"
	u := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5544", Path: "/fabric_fixture", User: url.UserPassword("fixture", marker), RawQuery: "sslmode=disable"}
	unsetConnectionEnv(t, "FABRIC_DSN")
	unsetConnectionEnv(t, "FABRIC_DSN_PASSWORD_FILE")
	unsetConnectionEnv(t, "FABRIC_DSN_FILE_KEY")
	t.Setenv("FABRIC_DSN_FILE", syntheticConnectionFile(t, u.String()))
	dsn, credential, err := appConnection()
	if err != nil {
		t.Fatal("app file reference refused")
	}
	defer credential.Close()
	if strings.Contains(dsn, marker) {
		t.Fatal("app connection reference exposed credential")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil || cfg.Password != marker || cfg.Database != "fabric_fixture" {
		t.Fatal("app did not load its exact reference")
	}
}

func TestReviewFileReferenceIsOwnedAndRemovedFromEnvironment(t *testing.T) {
	const marker = "synthetic-review-fixture"
	u := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/fabric_fixture", User: url.UserPassword("fixture", marker), RawQuery: "sslmode=disable"}
	unsetConnectionEnv(t, "FABRIC_REVIEW_DSN")
	unsetConnectionEnv(t, "FABRIC_REVIEW_DSN_PASSWORD_FILE")
	unsetConnectionEnv(t, "FABRIC_REVIEW_DSN_FILE_KEY")
	t.Setenv("FABRIC_REVIEW_DSN_FILE", syntheticConnectionFile(t, "FABRIC_REVIEW_DSN="+u.String()+"\n"))
	t.Setenv("FABRIC_REVIEW_DSN_FILE_KEY", "FABRIC_REVIEW_DSN")
	dsn, credential, err := takeReviewDSN()
	if err != nil {
		t.Fatal("review file reference refused")
	}
	defer credential.Close()
	for _, name := range []string{"FABRIC_REVIEW_DSN", "FABRIC_REVIEW_DSN_FILE", "FABRIC_REVIEW_DSN_FILE_KEY"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatal("review source metadata survived in worker parent")
		}
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil || cfg.Password != marker {
		t.Fatal("review reference was released too early")
	}
	credential.Close()
	if _, err := credential.DSN(); err == nil {
		t.Fatal("closed review reference remained usable")
	}
}

func TestWorkerPortsResolveCustomRunnerReference(t *testing.T) {
	unsetConnectionEnv(t, "FABRIC_DSN_FILE")
	unsetConnectionEnv(t, "FABRIC_DSN")
	unsetConnectionEnv(t, "FABRIC_DSN_PASSWORD_FILE")
	unsetConnectionEnv(t, "FABRIC_DSN_FILE_KEY")
	t.Setenv("FABRIC_RUNNER_DSN_ENV", "CUSTOM_RUNNER")
	unsetConnectionEnv(t, "CUSTOM_RUNNER")
	unsetConnectionEnv(t, "CUSTOM_RUNNER_PASSWORD_FILE")
	unsetConnectionEnv(t, "CUSTOM_RUNNER_FILE_KEY")
	t.Setenv("CUSTOM_RUNNER_FILE", syntheticConnectionFile(t, "postgres://fixture:synthetic@127.0.0.1:5545/fabric_fixture?sslmode=disable"))
	if got := workerTCPPorts(); !slices.Equal(got, []int{443, 5545}) {
		t.Fatal("custom runner reference port was not allowed")
	}
	t.Setenv("CUSTOM_RUNNER_FILE_KEY", "FABRIC_DSN")
	if got := workerTCPPorts(); !slices.Equal(got, []int{443}) {
		t.Fatal("invalid runner purpose granted database access")
	}
}

func TestAppPasswordFileReusesProtectedSource(t *testing.T) {
	for _, name := range []string{"FABRIC_DSN", "FABRIC_DSN_FILE", "FABRIC_DSN_FILE_KEY", "FABRIC_DSN_PASSWORD_FILE"} {
		unsetConnectionEnv(t, name)
	}
	const marker = "synthetic-reused-password"
	path := syntheticConnectionFile(t, " \t"+marker+"\n")
	// The existing role provisioning loader and the connection both consume
	// this single source. Neither writes another operational credential file.
	effective, err := readSecret(path)
	if err != nil || effective != marker {
		t.Fatal("role password loader source mismatch")
	}
	t.Setenv("FABRIC_DSN", "postgres://fixture@127.0.0.1:5432/fabric_fixture?sslmode=disable")
	t.Setenv("FABRIC_DSN_PASSWORD_FILE", path)
	dsn, credential, err := appConnection()
	if err != nil {
		t.Fatal("app password-file connection refused")
	}
	defer credential.Close()
	u, err := url.Parse(dsn)
	if err != nil || u.Query().Get("passfile") == "" || strings.Contains(dsn, marker) {
		t.Fatal("app password reference is not secret-free")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil || cfg.Password != effective {
		t.Fatal("client and role loader did not reuse the same source")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatal("password-only reference created another persistent source")
	}
}
