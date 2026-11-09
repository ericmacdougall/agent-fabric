package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"fabric/internal/store"
)

// streamsMain is `fab streams held [--scan] [--limit N] [--after MISSION]`.
// It is read-only and paged. Without --scan it lists the held missions from
// the hold rows without decoding any payload; --scan is the only full decode
// of history (it finds missions this build cannot read that are not yet held,
// which the rehearsal needs). Output carries a mission id, an event kind name,
// a reason class and a time, never payload text.
func streamsMain(args []string) error {
	if len(args) == 0 || args[0] != "held" {
		return usageError{fmt.Errorf("usage: fab streams held [--scan] [--limit N] [--after MISSION]")}
	}
	fs := flag.NewFlagSet("streams held", flag.ContinueOnError)
	scan := fs.Bool("scan", false, "decode every event of every mission (read-only, paged)")
	limit := fs.Int("limit", 100, "page size (missions)")
	after := fs.String("after", "", "list missions after this id")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed the usage; that is not a failure
		}
		return err
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("usage: fab streams held [--scan] [--limit N] [--after MISSION] (unexpected argument)")}
	}
	if *limit < 1 || *limit > 10000 {
		return usageError{fmt.Errorf("--limit must be between 1 and 10000")}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		if !*scan {
			infos, err := st.HeldInfos(ctx, *after, *limit)
			if err != nil {
				return err
			}
			for _, h := range infos {
				fmt.Printf("%s\t%s\t%s\t%s\n", h.Mission, h.Kind, h.Reason, h.Since.UTC().Format("2006-01-02T15:04:05Z"))
			}
			n, err := st.HeldCount(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("held streams: %d\n", n)
			return nil
		}
		found, cursor := 0, *after
		for {
			infos, next, err := st.ScanUndecodable(ctx, cursor, *limit)
			if err != nil {
				return err
			}
			for _, h := range infos {
				state := "held"
				if !h.Held {
					state = "not-yet-held"
				}
				fmt.Printf("%s\t%s\t%s\t%s\n", h.Mission, h.Kind, h.Reason, state)
				found++
			}
			if next == "" {
				break
			}
			cursor = next
		}
		fmt.Printf("held streams: %d\n", found)
		return nil
	})
}
