package main

// fab ladder (goodqa#15324). Two stages share the command:
//
//	S1a  fab ladder status --state FILE --policy FILE [--json]
//	     the model ladder computed from an escalation.py state file; no database.
//	S1b  fab ladder record --task T --event FILE
//	     appends ONE record that escalation.py already classified to the task's
//	     Fabric ladder mission (append-only events, one fixed actor).
//	     fab ladder status --task T --policy FILE [--json]
//	     the same status derived from the stored events, in a READ ONLY
//	     transaction. The decision it prints is advisory until S3 (goodqa#15135);
//	     the harness hook stays the enforcer.
//
// Fabric never classifies a receipt and never reads a receipt body: it stores
// the shape escalation.py produced. Output carries ids, classes, fingerprints,
// heads and counts only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/ladder"
	"fabric/internal/metrics"
	"fabric/internal/store"
	"fabric/internal/tui"
)

const ladderUsage = "ladder: usage: fab ladder status (--state FILE --policy FILE | --task T --policy FILE) [--json]  |  fab ladder record --task T --event FILE"

func ladderMain(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(ladderUsage)}
	}
	switch args[0] {
	case "status":
		return ladderStatusMain(args[1:])
	case "record":
		return ladderRecordMain(args[1:])
	}
	return usageError{errors.New(ladderUsage)}
}

func ladderStatusMain(args []string) error {
	fs := flag.NewFlagSet("ladder status", flag.ContinueOnError)
	statePath := fs.String("state", "", "escalation.py state file (file mode)")
	policyPath := fs.String("policy", "", "model_tiers.json")
	task := fs.String("task", "", "task key (database mode)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return usageError{fmt.Errorf("ladder status: %v", err)}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("ladder status: unexpected argument %q", fs.Arg(0))}
	}
	switch {
	case *statePath != "" && *task != "":
		return usageError{errors.New("ladder status: --state and --task are different modes")}
	case *task != "":
		if *policyPath == "" {
			return usageError{errors.New("ladder status: --policy is required")}
		}
		return ladderStatusDB(*task, *policyPath, *asJSON)
	case *statePath == "" || *policyPath == "":
		return usageError{errors.New("ladder status: --state and --policy are required (or --task and --policy)")}
	}
	st, err := ladder.StatusFromFiles(*statePath, *policyPath)
	if ladder.ExitCode(err) == 3 {
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

// ladderRefusal reports whether err is a refusal of the input (exit 3).
func ladderRefusal(err error) bool {
	for _, target := range []error{domain.ErrInvalidReceipt, domain.ErrIdempotencyConflict, domain.ErrTooManyReceipts, domain.ErrInvalidID,
		domain.ErrInvalidTransition, domain.ErrLadderMission, domain.ErrNotLadderMission, ladder.ErrInput, ladder.ErrPolicy} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func ladderStatusDB(task, policyPath string, asJSON bool) error {
	id, err := domain.LadderMissionID(task)
	if err != nil {
		return usageError{err}
	}
	body, err := ladder.ReadInput(policyPath, ladder.MaxPolicy)
	if err != nil {
		return usageError{err}
	}
	pol, err := ladder.DecodePolicy(policyPath, body)
	if err != nil {
		return usageError{err}
	}
	dsn, credential, err := appConnection()
	if err != nil {
		return err
	}
	defer credential.Close()
	// The same read-only pool the TUI uses: every statement runs in a READ ONLY
	// transaction, so this path cannot write whatever role the DSN names.
	pool, err := tui.OpenPool(dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	var evs []domain.Event
	err = metrics.ReadOnly(context.Background(), pool, 20*time.Second, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		evs, err = store.ReadLadderEvents(ctx, tx, id)
		return err
	})
	if err != nil {
		if ladderRefusal(err) {
			return usageError{err}
		}
		return err
	}
	ts, err := ladder.StatusOf(pol, task, evs)
	if err != nil {
		if ladderRefusal(err) {
			return usageError{err}
		}
		return err
	}
	out, err := ladder.FormatTask(ts, asJSON)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// maxLadderEvent bounds the record file; a record is a few hundred bytes.
const maxLadderEvent = 64 * 1024

func ladderRecordMain(args []string) error {
	fs := flag.NewFlagSet("ladder record", flag.ContinueOnError)
	task := fs.String("task", "", "task key")
	eventPath := fs.String("event", "", "JSON file: one record escalation.py classified")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return usageError{fmt.Errorf("ladder record: %v", err)}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("ladder record: unexpected argument %q", fs.Arg(0))}
	}
	if *task == "" || *eventPath == "" {
		return usageError{errors.New("ladder record: --task and --event are required")}
	}
	if _, err := domain.LadderMissionID(*task); err != nil {
		return usageError{err}
	}
	body, err := ladder.ReadInput(*eventPath, maxLadderEvent)
	if err != nil {
		return usageError{err}
	}
	rec, err := decodeLadderRecord(body)
	if err != nil {
		return usageError{err}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		resp, err := st.RecordLadderReceipt(ctx, *task, rec)
		if err != nil {
			if ladderRefusal(err) {
				return usageError{err}
			}
			return err
		}
		e := resp.Events[0]
		what := "receipt"
		if e.Kind == domain.EvLadderTopApproved {
			what = "approval"
		}
		fmt.Printf("recorded %s task %s seq %d step %s class %s replayed=%t\n", what, *task, resp.LastSeq, rec.Step, classOrDash(rec.Class), resp.Replayed)
		return nil
	})
}

func classOrDash(c string) string {
	if c == "" {
		return "-"
	}
	return c
}

// decodeLadderRecord is strict: unknown fields, a second JSON value and a
// non-object are refused, so a typo never becomes a silently dropped field.
func decodeLadderRecord(body []byte) (domain.LadderRecord, error) {
	var rec domain.LadderRecord
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, fmt.Errorf("%w: ladder record is not a record object", ladder.ErrInput)
	}
	// CI-7: dec.More() is false for a stray "]" or "}", so require the end of
	// input: Token() returns io.EOF only when nothing but whitespace follows.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return rec, fmt.Errorf("%w: ladder record file holds data after the record", ladder.ErrInput)
	}
	if err := domain.ValidateLadderRecord(&rec); err != nil {
		return rec, err
	}
	return rec, nil
}
