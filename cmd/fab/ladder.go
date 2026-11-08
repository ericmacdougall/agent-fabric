package main

// fab ladder status (goodqa#15324, slice S1a): the harness model ladder
// computed by Fabric from an escalation.py state file and the tier policy.
// It is read-only and needs no database. The logic lives in
// internal/ladder (StatusFromFiles, Format) and is tested there.

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"fabric/internal/ladder"
)

func ladderMain(args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return usageError{errors.New("ladder: usage: fab ladder status --state FILE --policy FILE [--json]")}
	}
	fs := flag.NewFlagSet("ladder status", flag.ContinueOnError)
	statePath := fs.String("state", "", "escalation.py state file")
	policyPath := fs.String("policy", "", "model_tiers.json")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args[1:]); err != nil {
		return usageError{fmt.Errorf("ladder status: %v", err)}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("ladder status: unexpected argument %q", fs.Arg(0))}
	}
	if *statePath == "" || *policyPath == "" {
		return usageError{errors.New("ladder status: --state and --policy are required")}
	}
	st, err := ladder.StatusFromFiles(*statePath, *policyPath)
	if errors.Is(err, ladder.ErrInput) || errors.Is(err, ladder.ErrPolicy) {
		// A refused input is a refusal (exit 3), not an internal error
		// (S1A-RT-9b), so a caller can tell bad files from a crash.
		return usageError{err}
	}
	if err != nil {
		return err
	}
	out, err := ladder.Format(st, *asJSON)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
