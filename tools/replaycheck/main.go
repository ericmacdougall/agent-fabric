// Command replaycheck prints, for every mission, the digest of the state the
// binary's own reader rebuilds from the log and whether the stored snapshot is
// jsonb-equal to it (goodqa#15372, AC-1(b)). Built from the live commit and
// from the candidate commit with `go build`, run against the same copy of live
// data, the two TSVs must be identical.
//
// It uses only store.Open, VerifiedSnapshot, reducer.Digest and one read-only
// SQL comparison, so the same source builds at the live commit (the old
// reader) and at the candidate (the v2 reader). The first line of the output
// records the commit the binary was built from; tools/replaycheck/diff.py
// refuses to compare two files with the same or a missing commit.
//
// It reads only: the connection is read-only, nothing is written, and no DSN,
// payload text or token is printed. FABRIC_DSN names a COPY of live data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"fabric/internal/domain"
	"fabric/internal/reducer"
	"fabric/internal/store"
)

func commit() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "unknown"
}

// run writes the TSV for every mission of st, paged by id.
func run(ctx context.Context, st *store.Store, out io.Writer, build string) error {
	fmt.Fprintf(out, "# build %s\n", build)
	fmt.Fprintln(out, "mission\tdigest\tjsonb_eq\tstatus")
	after := ""
	for {
		rows, err := st.Pool().Query(ctx, `SELECT id FROM missions WHERE id > $1 ORDER BY id LIMIT 200`, after)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			rebuilt, _, verr := st.VerifiedSnapshot(ctx, domain.MissionID(id))
			digest, eq, status := "-", "-", "ok"
			switch {
			case verr == nil:
			case errors.Is(verr, domain.ErrUnsupportedEvent):
				status = "unsupported"
			case rebuilt.ID != "":
				status = "mismatch"
			default:
				status = "error"
			}
			if rebuilt.ID != "" {
				digest = reducer.Digest(rebuilt)
				state, _ := json.Marshal(rebuilt)
				var same bool
				if err := st.Pool().QueryRow(ctx, `SELECT state = $2::jsonb FROM missions WHERE id = $1`, id, state).Scan(&same); err == nil {
					eq = fmt.Sprint(same)
				}
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", id, digest, eq, status)
			after = id
		}
	}
}

func main() {
	dsn := os.Getenv("FABRIC_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "replaycheck: FABRIC_DSN (a copy of live data) is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replaycheck: open failed")
		os.Exit(1)
	}
	defer st.Close()
	if err := run(ctx, st, os.Stdout, commit()); err != nil {
		fmt.Fprintln(os.Stderr, "replaycheck: read failed")
		os.Exit(1)
	}
}
