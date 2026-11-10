package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/control/ingest"
)

// controlIngestMain is `fab control ingest --source NAME < events.jsonl`:
// validate mc.event/1 lines from an external producer and store them in
// control.ingested_events (idempotent by id). Counts and rejection reasons go
// to stderr. A call over a limit (1 MiB, 2000 lines) is refused whole, exit 3,
// with nothing written; a bad line is rejected alone and the rest continue
// (exit 0 with the counts). The source is pinned by the flag, never by the
// lines, and "fabric" is reserved for `fab control export`.
func controlIngestMain(args []string) error {
	fs := flag.NewFlagSet("control ingest", flag.ContinueOnError)
	source := fs.String("source", "", "producer name, ^[a-z][a-z0-9_-]{0,31}$ (not \"fabric\"); every id must start with NAME:")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err}
	}
	if fs.NArg() != 0 || *source == "" {
		return usageError{fmt.Errorf("usage: fab control ingest --source NAME < events.jsonl")}
	}
	if err := ingest.ValidSource(*source); err != nil {
		return usageError{err}
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
	_, err = ingest.Run(ctx, conn, os.Stdin, *source, os.Stderr)
	if errors.Is(err, ingest.ErrCallTooLarge) {
		return usageError{err}
	}
	return err
}
