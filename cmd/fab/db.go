package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"fabric/internal/store"
)

// dbMain: `fab db roles` applies the least-privilege roles (goodqa#14929)
// as the owner, after `fab migrate`; `fab db check-runner` verifies the
// runner DSN (red-team G2). Passwords come from files and are never printed.
func dbMain(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fab db roles|check-runner|capacity ...")
	}
	switch args[0] {
	case "roles":
		fs := flag.NewFlagSet("db roles", flag.ContinueOnError)
		prefix := fs.String("prefix", "fabric", "role name prefix")
		appPW := fs.String("app-password-file", "", "file holding the app role password (empty: NOLOGIN)")
		runnerPW := fs.String("runner-password-file", "", "file holding the runner role password (empty: NOLOGIN)")
		controlOnly := fs.Bool("control-only", false, "manage only the Control Room roles (never the fabric roles, scheduler_role or their passwords)")
		exportPW := fs.String("export-password-file", "", "with --control-only: export role password file (empty: NOLOGIN)")
		ingestPW := fs.String("ingest-password-file", "", "with --control-only: ingest role password file (empty: NOLOGIN)")
		servicePW := fs.String("service-password-file", "", "with --control-only: service role password file (empty: NOLOGIN)")
		verify := fs.Bool("verify", false, "with --control-only: report how the control roles differ from the design (read only)")
		remove := fs.Bool("remove", false, "with --control-only: print the plan to drop the control roles (acts only with --confirm)")
		confirm := fs.Bool("confirm", false, "with --control-only --remove: really drop the roles")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *controlOnly {
			if *appPW != "" || *runnerPW != "" {
				return usageError{errors.New("fab db roles --control-only never touches the fabric roles: drop --app-password-file and --runner-password-file")}
			}
			return controlRolesMain(*prefix, *exportPW, *ingestPW, *servicePW, *verify, *remove, *confirm)
		}
		if *exportPW != "" || *ingestPW != "" || *servicePW != "" || *verify || *remove || *confirm {
			return usageError{errors.New("the control role flags need --control-only")}
		}
		app, err := readSecret(*appPW)
		if err != nil {
			return err
		}
		runner, err := readSecret(*runnerPW)
		if err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			r := store.Roles{Prefix: *prefix}
			if err := st.ApplyRoles(ctx, r, app, runner); err != nil {
				return err
			}
			fmt.Printf("roles applied: %s, %s, %s\n", r.Scheduler(), r.App(), r.Runner())
			return nil
		})
	case "check-runner":
		fs := flag.NewFlagSet("db check-runner", flag.ContinueOnError)
		env := fs.String("dsn-env", "FABRIC_RUNNER_DSN", "environment variable holding the runner DSN")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		dsn := os.Getenv(*env)
		if dsn == "" {
			return fmt.Errorf("%s is not set", *env)
		}
		ctx := context.Background()
		st, err := store.Open(ctx, dsn)
		if err != nil {
			return errors.New("runner DSN: cannot connect")
		}
		defer st.Close()
		if err := st.CheckRunnerRole(ctx); err != nil {
			return err
		}
		fmt.Println("runner role ok")
		return nil
	case "capacity":
		fs := flag.NewFlagSet("db capacity", flag.ContinueOnError)
		res := fs.String("resource", "", "resource name")
		unit := fs.String("unit", "slots", "unit")
		capacity := fs.Int64("capacity", -1, "capacity")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withStore(func(ctx context.Context, st *store.Store) error {
			return setCapacity(ctx, st, *res, *unit, *capacity)
		})
	}
	return fmt.Errorf("fab db: unknown subcommand %q", args[0])
}

type capacityStore interface {
	SetCapacity(ctx context.Context, resource, unit string, capacity int64) error
	Capacity(ctx context.Context, resource string) (capacity, held int64, err error)
}

// setCapacity runs `fab db capacity`. A domain refusal (bad input, a unit
// change, a capacity below what is held) changed nothing, so it becomes a
// *store.Refusal and main exits 3, not 1 (review F7, goodqa#15074).
func setCapacity(ctx context.Context, st capacityStore, res, unit string, capacity int64) error {
	if err := st.SetCapacity(ctx, res, unit, capacity); err != nil {
		return refusal(err)
	}
	c, h, err := st.Capacity(ctx, res)
	if err != nil {
		// The set has committed; only the readback failed (exit 1). A rerun
		// with the same values is idempotent.
		return fmt.Errorf("capacity set; readback failed: %w", err)
	}
	fmt.Printf("%s: capacity %d, held %d\n", res, c, h)
	return nil
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("password file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// holdFlags parses repeated --hold resource:unit:amount values.
type holdFlags []store.HoldItem

func (h *holdFlags) String() string { return fmt.Sprint(len(*h)) }
func (h *holdFlags) Set(v string) error {
	p := strings.Split(v, ":")
	if len(p) != 3 {
		return fmt.Errorf("--hold %q: want resource:unit:amount", v)
	}
	n, err := strconv.ParseInt(p[2], 10, 64)
	if err != nil {
		return fmt.Errorf("--hold %q: %w", v, err)
	}
	*h = append(*h, store.HoldItem{Resource: p[0], Unit: p[1], Amount: n})
	return nil
}
