package main

import (
	"context"
	"errors"
	"fmt"

	"fabric/internal/control"
	"fabric/internal/store"
)

// controlBootstrapMain runs `fab control bootstrap`: it creates the Control
// Room's schema once, as the database owner (goodqa#15586). It never runs at
// runtime, never touches fabric_schema and is idempotent. The dispatcher in
// control.go calls it.
func controlBootstrapMain(args []string) error {
	if len(args) != 0 {
		return usageError{errors.New("usage: fab control bootstrap (no arguments; connects with FABRIC_DSN as the database owner)")}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		if err := control.Bootstrap(ctx, st.Pool()); err != nil {
			return err
		}
		fmt.Printf("control schema ready (version %d, definition %s)\n", control.SchemaVersion(), control.SchemaSHA256()[:12])
		return nil
	})
}
