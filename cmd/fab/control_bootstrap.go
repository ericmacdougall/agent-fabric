package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"fabric/internal/control"
	"fabric/internal/store"
)

// controlBootstrapMain runs `fab control bootstrap`: it creates the Control
// Room's schema once, as the database owner (goodqa#15586). It never runs at
// runtime, never touches fabric_schema and is idempotent. With --check-prefix P
// it then verifies the control roles of that prefix read-only (spec goodqa#15623
// AC-7) and fails when they differ from the design. The dispatcher in control.go
// calls it.
func controlBootstrapMain(args []string) error {
	fs := flag.NewFlagSet("control bootstrap", flag.ContinueOnError)
	checkPrefix := fs.String("check-prefix", "", "after the bootstrap, verify the control roles with this prefix (read only); empty: skip")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usageError{errors.New("usage: fab control bootstrap [--check-prefix PREFIX] (connects with FABRIC_DSN as the database owner)")}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		if err := control.Bootstrap(ctx, st.Pool()); err != nil {
			return err
		}
		fmt.Printf("control schema ready (version %d, definition %s)\n", control.SchemaVersion(), control.SchemaSHA256()[:12])
		if *checkPrefix == "" {
			return nil
		}
		problems, notes, err := control.VerifyRoles(ctx, st.Pool(), control.Roles{Prefix: *checkPrefix})
		if err != nil {
			return err
		}
		for _, n := range notes {
			fmt.Println("note:", n)
		}
		for _, p := range problems {
			fmt.Println("PROBLEM:", p)
		}
		if len(problems) > 0 {
			return control.ErrVerify
		}
		fmt.Println("control roles ok")
		return nil
	})
}
