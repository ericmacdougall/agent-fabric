package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

// daemonPolicyCheck is fabd's start check; see daemonMain.
func daemonPolicyCheck() error {
	if !m1build.Enabled {
		return nil
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		return checkEffectPolicy(ctx, effects.New(st))
	})
}

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
