package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/m1build"
	"fabric/internal/store"
)

// policyMain (goodqa#16039): a read-only interface for the M1 comment
// policy, separate from `fab version` (which stays hex-only).
//
//	fab policy build    prints m1 or non-m1; exit 0 only for an M1 build
//	fab policy marker   prints the database marker: m1, absent or unknown;
//	                    exit 0 only for m1 (a missing marker never reads as m1)
//
// The RR-5 inventory uses `fab policy build`: a binary built before this
// code has no `policy` command and fails it.
func policyMain(args []string) error {
	if len(args) != 1 {
		return usageError{errors.New("usage: fab policy build|marker")}
	}
	switch args[0] {
	case "build":
		fmt.Println(m1build.Name())
		if !m1build.Enabled {
			return policyNotM1{"this binary is not an M1 build"}
		}
		return nil
	case "marker":
		return withStore(func(ctx context.Context, st *store.Store) error {
			v, present, err := effects.ReadMarker(ctx, effects.New(st))
			if err != nil {
				return err
			}
			switch {
			case !present:
				fmt.Println("absent")
				return policyNotM1{"the database has no comment_policy marker"}
			case v != effects.PolicyMarkerM1:
				fmt.Println("unknown")
				return policyNotM1{fmt.Sprintf("the comment_policy marker holds an unknown value (%d bytes)", len(v))}
			}
			fmt.Println(effects.PolicyMarkerM1)
			return nil
		})
	}
	return usageError{fmt.Errorf("policy: unknown subcommand %q (build|marker)", args[0])}
}

// policyNotM1 is a negative policy report: exit 3, nothing changed.
type policyNotM1 struct{ msg string }

func (p policyNotM1) Error() string { return p.msg }

// checkEffectPolicy is the start check of every mutating `fab effect`
// subcommand (M1 comment policy table, last column).
func checkEffectPolicy(ctx context.Context, b *effects.Broker) error {
	return b.CheckPolicy(ctx)
}

// daemonPolicyCheck is fabd's start check; see daemonMain. An M1 build
// refuses a database without the comment_policy marker (NEG-23). A non-M1
// build that dispatches refuses a database WITH the comment_policy=m1
// marker (goodqa#15966 item 4, PR erratum E12): it would START attempts
// without the M1 token launch. With --no-dispatch it starts nothing, so it
// may run there, and says so once.
func daemonPolicyCheck(noDispatch bool) error {
	if m1build.Enabled {
		return withStore(func(ctx context.Context, st *store.Store) error {
			return checkEffectPolicy(ctx, effects.New(st))
		})
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		v, ok, err := st.CommentPolicyMarker(ctx)
		if err != nil {
			return fmt.Errorf("read the comment_policy marker: %w", err)
		}
		if !ok || v != effects.PolicyMarkerM1 {
			return nil
		}
		if noDispatch {
			fmt.Fprintln(os.Stderr, "fabd: non-M1 build on a database with the comment_policy=m1 marker: --no-dispatch, so no attempt is started")
			return nil
		}
		return &effects.PolicyRefusal{Code: CodeMarkedNonM1, Detail: "this is not an M1 build and the database carries the fabric_settings comment_policy=m1 marker; a dispatching fabd here would START attempts without the M1 token launch; run the M1 build, or --no-dispatch"}
	})
}

// CodeMarkedNonM1 is the refusal of a dispatching non-M1 fabd on an
// M1-marked database (exit 3).
const CodeMarkedNonM1 = "m1_marked_non_m1_build"

// eventsMain: `fab events --mission M [--json]` (goodqa#15966 AC-12): the
// mission's events with its effect rows joined at read time, ordered by
// time; effect rows are labelled effect:{state}. Read-only.
func eventsMain(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := domain.ParseMissionID(*mission); err != nil {
		return usageError{fmt.Errorf("events: --mission: %w", err)}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		rows, err := effects.Timeline(ctx, st, domain.MissionID(*mission))
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(rows)
		}
		for _, r := range rows {
			line := r.At.UTC().Format("2006-01-02T15:04:05.000Z") + " " + r.Label
			if r.Seq > 0 {
				line += fmt.Sprintf(" seq=%d", r.Seq)
			}
			if r.Attempt != "" {
				line += " attempt=" + r.Attempt
			}
			if r.Effect != "" {
				line += " effect=" + r.Effect + " key=" + r.Key
			}
			fmt.Println(line)
		}
		return nil
	})
}

// m1RecordsDir is fabd's AC-29 records directory ({run area}/records). The
// run area is under the work root until W3's fabd-only run area lands.
func m1RecordsDir(workRoot string) string { return filepath.Join(workRoot, "run-area", "records") }
