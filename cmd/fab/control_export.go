package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/control/export"
)

// controlExportMain is `fab control export [--since CURSOR]`: read-only
// mc.event/1 lines for Mission Control, then one {"cursor":"..."} line. A
// refused cursor is exit 3 with nothing written to stdout.
func controlExportMain(args []string) error {
	fs := flag.NewFlagSet("control export", flag.ContinueOnError)
	since := fs.String("since", "", "resume after this opaque cursor (omit for the beginning)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("usage: fab control export [--since CURSOR] (unexpected argument)")}
	}
	dsn := os.Getenv("FABRIC_DSN")
	if dsn == "" {
		return errors.New("FABRIC_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	res, err := export.Export(ctx, conn, *since, os.Stdout)
	if errors.Is(err, export.ErrBadCursor) {
		return usageError{err}
	}
	if err != nil {
		return err
	}
	if res.More {
		fmt.Fprintf(os.Stderr, "fab control export: output capped at %d bytes after %d events; call again with the printed cursor\n", export.MaxOutput, res.Events)
	}
	return nil
}
