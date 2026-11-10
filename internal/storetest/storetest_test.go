package storetest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/connref"
	"fabric/internal/store"
)

// This package's regression is intentionally a real disposable-Postgres test.
// A compile-only or synthetic connref check cannot prove cluster role cleanup.
func TestMain(m *testing.M) {
	if !connref.HasReferenceConfig("FABRIC_TEST_DSN") && os.Getenv(ChildDSNEnv) == "" {
		fmt.Fprintln(os.Stderr, "cleanup control requires explicit test credential references")
		os.Exit(2)
	}
	os.Exit(Main(m, "fixture_cleanup"))
}

const failureChildEnv = "FABRIC_FIXTURE_FAILURE_CHILD"
const failureRootEnv = "FABRIC_FIXTURE_FAILURE_ROOT"

func TestAppRunnerFixtureFailureCleansOwnedRoles(t *testing.T) {
	if phase := os.Getenv(failureChildEnv); phase != "" {
		runFixtureFailureChild(t, phase)
		return
	}
	if !connref.HasReferenceConfig("FABRIC_TEST_DSN") {
		t.Fatal("the cleanup control requires an explicit test credential reference")
	}
	parent, err := store.Open(context.Background(), DSN)
	if err != nil {
		t.Fatal("open cleanup-control database failed")
	}
	defer parent.Close()
	for _, phase := range []string{"directory", "app", "runner"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			fixtureRoot := filepath.Join(root, "fixtures")
			if err := os.Mkdir(fixtureRoot, 0700); err != nil {
				t.Fatal("create control fixture root failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAppRunnerFixtureFailureCleansOwnedRoles$", "-test.count=1")
			child.Env = controlChildEnv(phase, root)
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal("start cleanup fault child failed")
			}
			roles := store.Roles{Prefix: fmt.Sprintf("fabric_s%d", child.Process.Pid)}
			// Recovery is scoped to this exact child, and runs only after the
			// zero-role assertion has observed whether helper cleanup worked.
			t.Cleanup(func() {
				recoveryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				for _, role := range []string{roles.App(), roles.Runner(), roles.Scheduler()} {
					parent.Pool().Exec(recoveryCtx, "DROP OWNED BY "+role)       //nolint:errcheck
					parent.Pool().Exec(recoveryCtx, "DROP ROLE IF EXISTS "+role) //nolint:errcheck
				}
			})
			waitErr := child.Wait()
			if ctx.Err() != nil {
				t.Fatal("cleanup fault child exceeded its owned deadline")
			}
			exit, ok := waitErr.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatal("cleanup fault child did not report its intended test failure")
			}
			message := map[string]string{
				"directory": "create private credential fixture directory failed",
				"app":       "create app credential fixture failed",
				"runner":    "create runner credential fixture failed",
			}[phase]
			if !bytes.Contains(output.Bytes(), []byte(message)) {
				t.Fatal("cleanup fault did not reach the intended fixture failure")
			}
			proof, err := os.ReadFile(filepath.Join(root, "provisioned"))
			if err != nil || string(proof) != "roles=3;phase="+phase {
				t.Fatal("cleanup fault lacked positive proof of all three provisioned roles")
			}
			var remaining int
			checkCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := parent.Pool().QueryRow(checkCtx, "SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)", []string{roles.App(), roles.Runner(), roles.Scheduler()}).Scan(&remaining); err != nil {
				t.Fatal("read exact owned cleanup roles failed")
			}
			if remaining != 0 {
				t.Fatal("owned roles survived the fixture setup failure")
			}
			files, err := os.ReadDir(fixtureRoot)
			if err != nil || len(files) != 0 {
				t.Fatal("owned partial fixture files survived setup failure")
			}
		})
	}
}

func runFixtureFailureChild(t *testing.T, phase string) {
	t.Helper()
	root := os.Getenv(failureRootEnv)
	if root == "" {
		t.Fatal("cleanup fault child is not armed")
	}
	fixtureRoot := filepath.Join(root, "fixtures")
	owner := Open(t)
	roles := store.Roles{Prefix: fmt.Sprintf("fabric_s%d", os.Getpid())}
	proveProvisioned := func(directory string, partialFiles int) {
		t.Helper()
		var count int
		if err := owner.Pool().QueryRow(context.Background(), "SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)", []string{roles.App(), roles.Runner(), roles.Scheduler()}).Scan(&count); err != nil || count != 3 {
			t.Fatal("fault was not armed after all three roles were provisioned")
		}
		if directory != "" {
			files, err := os.ReadDir(directory)
			if err != nil || len(files) != partialFiles {
				t.Fatal("fault lacked the expected partial fixture state")
			}
		}
		if err := os.WriteFile(filepath.Join(root, "provisioned"), []byte("roles=3;phase="+phase), 0600); err != nil {
			t.Fatal("write cleanup fault proof failed")
		}
	}
	factories := fixtureFactories{
		directory: func(string) (string, error) {
			if phase == "directory" {
				proveProvisioned("", 0)
				return connref.NewFixtureDirectory(filepath.Join(fixtureRoot, "missing", "parent"))
			}
			return connref.NewFixtureDirectory(fixtureRoot)
		},
		credential: func(base string, purpose connref.Purpose, user, password, directory string) (*connref.Fixture, error) {
			if (phase == "app" && purpose == connref.App) || (phase == "runner" && purpose == connref.Runner) {
				partial := 0
				if purpose == connref.Runner {
					partial = 1
				}
				proveProvisioned(directory, partial)
				return connref.NewFixture(base, purpose, user, password, filepath.Join(directory, "missing"))
			}
			return connref.NewFixture(base, purpose, user, password, directory)
		},
	}
	if phase != "directory" && phase != "app" && phase != "runner" {
		t.Fatal("unknown cleanup fault phase")
	}
	appRunnerDSNs(t, owner, factories)
	t.Fatal("real fixture filesystem failure unexpectedly succeeded")
}

func controlChildEnv(phase, root string) []string {
	keys := map[string]string{ChildDSNEnv: DSN, failureChildEnv: phase, failureRootEnv: root}
	env := make([]string, 0, len(os.Environ())+len(keys))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := keys[name]; !replaced {
			env = append(env, entry)
		}
	}
	for _, key := range []string{ChildDSNEnv, failureChildEnv, failureRootEnv} {
		env = append(env, key+"="+keys[key])
	}
	return env
}
