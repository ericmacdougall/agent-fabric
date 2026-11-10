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

func controlCursorRollbackMain(args []string) error {
	if len(args) != 1 {
		return usageError{errors.New("usage: fab control cursor-rollback CURSOR (caught-up fx2 only; does not seed a consumer)")}
	}
	cursor, err := export.RollbackCursor(args[0])
	if err != nil {
		return usageError{err}
	}
	_, err = fmt.Fprintln(os.Stdout, cursor)
	return err
}

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
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("usage: fab control export [--since CURSOR] (unexpected argument)")}
	}
	dsn, credential, err := appConnection()
	if err != nil {
		return err
	}
	defer credential.Close()
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
	if res.Lagging > 0 {
		fmt.Fprintf(os.Stderr, "fab control export: LAGGING: %d committed event(s) are held back by an older open transaction; \"no new events\" does not mean caught up, retry later\n", res.Lagging)
	}
	if res.More {
		fmt.Fprintf(os.Stderr, "fab control export: output capped at %d bytes after %d events; call again with the printed cursor\n", export.MaxOutput, res.Events)
	}
	return nil
}
