package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/store"
)

// Release record classes: the pending record is written before the hold
// row is deleted and the final one after, so an audited release never
// depends on a write that happens after the hold is gone (red-team M-3).
const (
	releasePending = "operator_release_pending"
	releaseDone    = "operator_release"
	// releaseAckUnswept is the final class of a release whose run was never
	// swept (its bound values could not be loaded): the operator passed
	// --ack-unswept (spec v15, NEG-35).
	releaseAckUnswept = "operator_release_ack_unswept"
)

// unsweptClasses are the hold classes whose settle loaded no bound values,
// so no sweep ran over the worker's files.
var unsweptClasses = map[string]bool{credential.ClassMaterialUnavailable: true, credential.ClassRotatedMidAttempt: true}

// missionReleaseMain is `fab mission release --mission M --reason R`
// (goodqa#15966 AC-34, owner ruling R1): it lifts the operator hold fabd
// placed after a fail-closed M1 settle or a refused token. It runs with the
// app role, logs the reason, and records the release against the held
// attempt in fabd's records directory, which is required: without a
// writable records directory nothing is released.
func missionReleaseMain(args []string) error {
	fs := flag.NewFlagSet("mission release", flag.ContinueOnError)
	mission := fs.String("mission", "", "mission id")
	reason := fs.String("reason", "", "why the hold is lifted (required)")
	workRoot := fs.String("work-root", "", "fabd's --work-root; the records directory defaults to {work-root}/run-area/records")
	runArea := fs.String("run-area", "", "fabd's --run-area, when it set one; the records directory is {run-area}/records")
	recordsDir := fs.String("records-dir", "", "fabd's records directory, overriding --run-area and --work-root")
	ackUnswept := fs.Bool("ack-unswept", false, "acknowledge that the held run was never swept (required for a credential_material_unavailable or credential_rotated_mid_attempt hold)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m := domain.MissionID(*mission)
	if _, err := domain.ParseMissionID(*mission); err != nil {
		return usageError{fmt.Errorf("mission release: --mission: %w", err)}
	}
	r := strings.TrimSpace(*reason)
	if r == "" {
		return usageError{errors.New("mission release: --reason is required")}
	}
	if strings.ContainsAny(r, "\r\n") || len(r) > 200 {
		return usageError{errors.New("mission release: --reason must be one line of at most 200 bytes")}
	}
	dir := releaseRecordsDir(*recordsDir, *runArea, *workRoot)
	if dir == "" {
		return usageError{errors.New("mission release: --work-root, --run-area or --records-dir is required: the release is recorded in fabd's records directory")}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		return releaseOperatorHold(ctx, st, m, r, dir, *ackUnswept)
	})
}

func releaseRecordsDir(recordsDir, runArea, workRoot string) string {
	switch {
	case recordsDir != "":
		return filepath.Clean(recordsDir)
	case runArea != "":
		return filepath.Join(filepath.Clean(runArea), runAreaRecords)
	case workRoot != "":
		return filepath.Join(m1DefaultRunArea(filepath.Clean(workRoot)), runAreaRecords)
	}
	return ""
}

// releaseOperatorHold checks the hold and the records directory, writes the
// pending release record, deletes the hold row, then writes the final
// record. A failure before the delete leaves the hold in place; a failure
// of the final record is loud and non-zero, and the pending record remains.
func releaseOperatorHold(ctx context.Context, st *store.Store, m domain.MissionID, reason, recordsDir string, ackUnswept bool) error {
	if fi, err := os.Stat(recordsDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("mission release: records directory %s is missing; nothing released", recordsDir)
	}
	held, err := st.OperatorHeld(ctx, m)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("mission release: %s has no operator hold", m)
	}
	attempt, class, err := heldAttempt(ctx, st, m, recordsDir)
	if err != nil {
		return err
	}
	unswept := class == "" || unsweptClasses[class] // no hold record: fail closed
	if !unswept {
		rs, err := records.Read(recordsDir, attempt)
		if err != nil {
			return fmt.Errorf("mission release: records unreadable; nothing released: %w", err)
		}
		for _, r := range rs {
			unswept = unswept || (r.Kind == records.SweepIncomplete && r.Class == "material_unavailable")
		}
	}
	if unswept && !ackUnswept {
		return usageError{fmt.Errorf("mission release: %s's held run (%s) was never swept; pass --ack-unswept after checking its files; nothing released", m, orUnknown(class))}
	}
	final := releaseDone
	if unswept {
		final = releaseAckUnswept
	}
	rec := records.Record{Mission: m, Attempt: attempt, Kind: records.MissionRelease, Class: releasePending}
	if err := records.Append(recordsDir, rec); err != nil {
		return fmt.Errorf("mission release: release record not written; nothing released: %w", err)
	}
	released, err := st.ReleaseOperatorHold(ctx, m)
	if err != nil {
		return fmt.Errorf("mission release: hold not released (a pending release record remains): %w", err)
	}
	if !released {
		return fmt.Errorf("mission release: %s's hold vanished during release (a pending release record remains)", m)
	}
	fmt.Fprintf(os.Stderr, "fab: released the operator hold on %s (attempt %s): %s\n", m, attempt, reason)
	rec.Class = final
	if err := records.Append(recordsDir, rec); err != nil {
		fmt.Fprintf(os.Stderr, "fab: ERROR: the hold on %s IS RELEASED but its final release record was not written; only the pending record exists: %v\n", m, err)
		return fmt.Errorf("mission release: final release record not written: %w", err)
	}
	return nil
}

func orUnknown(class string) string {
	if class == "" {
		return "no hold record"
	}
	return class
}

// heldAttempt is the attempt the hold was placed for and the hold's class:
// the latest mission_hold record, else the mission's latest attempt in the
// store with no class (the hold record is best effort in fabd).
func heldAttempt(ctx context.Context, st *store.Store, m domain.MissionID, recordsDir string) (domain.AttemptID, string, error) {
	holds, err := records.List(recordsDir, records.MissionHold)
	if err != nil {
		return "", "", fmt.Errorf("mission release: records unreadable; nothing released: %w", err)
	}
	var best records.Record
	for _, h := range holds {
		if h.Mission == m && (best.Attempt == "" || h.Time.After(best.Time)) {
			best = h
		}
	}
	if best.Attempt != "" {
		return best.Attempt, best.Class, nil
	}
	var a string
	if err := st.Pool().QueryRow(ctx, `SELECT id FROM attempts WHERE mission_id = $1 ORDER BY started_at DESC, id DESC LIMIT 1`, string(m)).Scan(&a); err != nil {
		return "", "", fmt.Errorf("mission release: no attempt to record the release against; nothing released: %w", err)
	}
	return domain.AttemptID(a), "", nil
}
