//go:build linux

package connref

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/sys/unix"
)

const syntheticPassword = "fixture:only\\credential"

func source(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal("write synthetic source")
	}
	return p
}

func fixtureURL() string {
	u := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/postgres", User: url.UserPassword("fixture_admin", syntheticPassword)}
	u.RawQuery = "sslmode=disable"
	return u.String()
}

func load(t *testing.T) *Ref {
	t.Helper()
	t.Setenv("FABRIC_TEST_DSN", "")
	t.Setenv("FABRIC_TEST_DSN_FILE", source(t, fixtureURL()))
	r, err := ResolveEnv("FABRIC_TEST_DSN", TestAdmin)
	if err != nil {
		t.Fatal("resolve synthetic reference:", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func config(t *testing.T, r *Ref, db string) {
	t.Helper()
	dsn, err := r.DSN()
	if err != nil {
		t.Fatal("reference unavailable:", err)
	}
	if strings.Contains(dsn, syntheticPassword) || strings.Contains(dsn, url.QueryEscape(syntheticPassword)) || strings.Contains(dsn, "password=") {
		t.Fatal("secret appeared in connection reference")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse synthetic reference")
	}
	if cfg.Database != db || cfg.User != "fixture_admin" || cfg.Password != syntheticPassword {
		t.Fatal("exact database reference did not load the intended credential")
	}
}

func TestExactDerivedReferenceLifetime(t *testing.T) {
	parent := load(t)
	config(t, parent, "postgres")
	child, err := parent.ForDatabase("fabric_owned_fixture")
	if err != nil {
		t.Fatal("derive reference:", err)
	}
	defer child.Close()
	config(t, child, "fabric_owned_fixture")
	dsn, _ := child.DSN()
	u, _ := url.Parse(dsn)
	u.Path = "/different_database"
	other, err := pgconn.ParseConfig(u.String())
	if err != nil || other.Password != "" {
		t.Fatal("passfile matched an unlisted database")
	}
	if err := parent.Close(); err != nil {
		t.Fatal("close parent")
	}
	if err := parent.Close(); err != nil {
		t.Fatal("idempotent close")
	}
	config(t, child, "fabric_owned_fixture")
	if _, err := parent.ForDatabase("after_close"); err == nil {
		t.Fatal("derived from closed reference")
	}
	if _, err := parent.DSN(); err == nil {
		t.Fatal("read a closed reference")
	}
	if _, err := child.ForDatabase("*"); err == nil {
		t.Fatal("wildcard database accepted")
	}
}

func TestSealedPassfileAndClose(t *testing.T) {
	r := load(t)
	dsn, _ := r.DSN()
	u, _ := url.Parse(dsn)
	p := u.Query().Get("passfile")
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal("open synthetic transport")
	}
	st, err := f.Stat()
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("transport permissions")
	}
	seals, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&(unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL) != unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL {
		t.Fatal("transport is not fully sealed")
	}
	if _, err := f.Write([]byte("alter")); err == nil {
		t.Fatal("sealed transport was writable")
	}
	f.Close()
	r.Close()
	if _, err := os.Stat(p); err == nil {
		t.Fatal("closed reference descriptor survived")
	}
}

func TestPurposeIndependentOfConfigName(t *testing.T) {
	p := source(t, "FABRIC_DSN="+fixtureURL()+"\nFABRIC_RUNNER_DSN="+fixtureURL()+"\n")
	t.Setenv("CUSTOM_RUNNER", "")
	t.Setenv("CUSTOM_RUNNER_FILE", p)
	t.Setenv("CUSTOM_RUNNER_FILE_KEY", "FABRIC_DSN")
	if _, err := ResolveEnv("CUSTOM_RUNNER", Runner); err == nil {
		t.Fatal("runner accepted app field")
	}
	t.Setenv("CUSTOM_RUNNER_FILE_KEY", "FABRIC_RUNNER_DSN")
	r, err := ResolveEnv("CUSTOM_RUNNER", Runner)
	if err != nil {
		t.Fatal("custom runner reference refused:", err)
	}
	r.Close()
}

func TestUnsafeInputsRefuseWithoutEcho(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate":      "FABRIC_DSN=" + fixtureURL() + "\nFABRIC_DSN=" + fixtureURL() + "\n",
		"shell":          "FABRIC_DSN=$(printf forbidden)\n",
		"query password": fixtureURL() + "&password=forbidden",
		"unknown option": fixtureURL() + "&sslpassword=forbidden",
		"nul":            fixtureURL() + "\x00",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FABRIC_DSN", "")
			t.Setenv("FABRIC_DSN_FILE", source(t, body))
			if name == "duplicate" || name == "shell" {
				t.Setenv("FABRIC_DSN_FILE_KEY", "FABRIC_DSN")
			}
			_, err := ResolveEnv("FABRIC_DSN", App)
			if err == nil {
				t.Fatal("unsafe reference accepted")
			}
			if strings.Contains(err.Error(), "forbidden") || strings.Contains(err.Error(), syntheticPassword) {
				t.Fatal("unsafe input echoed")
			}
		})
	}
	for _, variant := range []string{"public", "symlink", "directory", "both", "pg override"} {
		t.Run(variant, func(t *testing.T) {
			p := source(t, fixtureURL())
			t.Setenv("FABRIC_DSN", "")
			if variant == "public" {
				os.Chmod(p, 0644)
			}
			if variant == "symlink" {
				q := p + "-link"
				os.Symlink(p, q)
				p = q
			}
			if variant == "directory" {
				p = filepath.Dir(p)
			}
			if variant == "both" {
				t.Setenv("FABRIC_DSN", "legacy")
			}
			if variant == "pg override" {
				t.Setenv("PGPASSWORD", "synthetic-override")
			}
			t.Setenv("FABRIC_DSN_FILE", p)
			if _, err := ResolveEnv("FABRIC_DSN", App); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestConcurrentDerivationAndClose(t *testing.T) {
	r := load(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			child, err := r.ForDatabase(fmt.Sprintf("fabric_fixture_%d", n))
			if err != nil {
				return
			} // parent close may linearize first
			defer child.Close()
			config(t, child, fmt.Sprintf("fabric_fixture_%d", n))
		}(i)
	}
	r.Close()
	wg.Wait()
}

// All derivations must actually happen before this parent is closed; a race
// where every derive merely refuses cannot satisfy this separate control.
func TestConcurrentReferencesRemainIndependent(t *testing.T) {
	r := load(t)
	ready := make(chan *Ref, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			child, err := r.ForDatabase(fmt.Sprintf("fabric_parallel_%d", n))
			if err != nil {
				ready <- nil
				return
			}
			ready <- child
		}(i)
	}
	wg.Wait()
	close(ready)
	refs := make([]*Ref, 0, 24)
	for child := range ready {
		if child == nil {
			t.Fatal("concurrent derivation failed while parent was open")
		}
		refs = append(refs, child)
	}
	r.Close()
	for _, child := range refs {
		dsn, err := child.DSN()
		if err != nil {
			t.Fatal("independent child was closed")
		}
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil || cfg.Password != syntheticPassword {
			t.Fatal("independent child lost its credential")
		}
		child.Close()
	}
}
