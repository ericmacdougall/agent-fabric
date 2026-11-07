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
		if err := fs.Parse(args[1:]); err != nil {
			return err
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
			if err := st.SetCapacity(ctx, *res, *unit, *capacity); err != nil {
				return err
			}
			c, h, err := st.Capacity(ctx, *res)
			fmt.Printf("%s: capacity %d, held %d\n", *res, c, h)
			return err
		})
	}
	return fmt.Errorf("fab db: unknown subcommand %q", args[0])
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
