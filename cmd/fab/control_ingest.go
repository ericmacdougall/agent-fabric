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
// with nothing written; a bad line is rejected alone and the rest continue:
// exit 0 when nothing was rejected, exit 4 when some lines were (counts on stderr). The source is pinned by the flag, never by the
// lines, and "fabric" is reserved for `fab control export`.
const ingestSessionLimit = 30 * time.Second

func controlIngestMain(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ingestSessionLimit)
	defer cancel()
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
	if err := ingest.CheckAllowedSource(*source); err != nil {
		return usageError{err}
	}
	dsn, credential, err := appConnection()
	if err != nil {
		return err
	}
	defer credential.Close()
	// The same absolute budget covers stdin, parsing, connection and storage.
	// Restore stdin flags and close our duplicate before opening a connection.
	input, err := newIngestInput(ctx, os.Stdin)
	if err != nil {
		return err
	}
	defer input.Close() //nolint:errcheck // explicitly checked before DB access below
	batch, err := ingest.ParseContext(ctx, input, *source)
	closeErr := input.Close()
	if closeErr != nil {
		return fmt.Errorf("ingest stdin cleanup: %w", closeErr)
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return usageError{errors.New("ingest session exceeded 30s wall-clock limit before database access")}
	}
	if errors.Is(err, ingest.ErrCallTooLarge) {
		return usageError{err}
	}
	if err != nil {
		return err
	}
	// No budget reset here. A DB timeout may have an uncertain write outcome,
	// so it remains an operational error, distinct from an input-only refusal.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx) //nolint:errcheck
	res, err := ingest.Finish(ctx, conn, batch, *source, os.Stderr)
	if err != nil {
		return err
	}
	if n := len(res.Batch.Rejected); n > 0 {
		return partialRejects{n}
	}
	return nil
}

// partialRejects is a call that completed (accepted lines are stored) but
// rejected some input lines: exit 4, so a pipeline can tell it from a clean
// run (0), a refusal that wrote nothing (3) and an error of unknown effect (1).
type partialRejects struct{ n int }

func (e partialRejects) Error() string {
	return fmt.Sprintf("completed with %d rejected line(s); the accepted lines were stored", e.n)
}
