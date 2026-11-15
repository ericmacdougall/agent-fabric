package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fabric/internal/crashpoint"
	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/records"
	"fabric/internal/store"
)

// releaseClass is the class of a release_authorized record; the reason and
// the ack are its own fields (spec v16).
const releaseClass = "operator_release"

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
	if len(r) > 200 || strings.IndexFunc(r, func(c rune) bool { return c < 0x20 || c > 0x7e }) >= 0 {
		return usageError{errors.New("mission release: --reason must be one line of printable ASCII, at most 200 bytes")}
	}
	if hit, class := credential.ScanUnbound(r); hit {
		return usageError{fmt.Errorf("mission release: --reason looks like it holds a credential (%s); refused", class)}
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

// releaseOperatorHold checks the records directory and the hold's class,
// then releases in one store transaction (spec v16 AC-34): the row is
// locked FOR UPDATE and its generation G read, the release_authorized
// record naming the mission, G, the reason and the ack is written durably
// (lock, temporary file, fsync, rename, directory fsync), and only then is
// the row deleted where the generation is still G. Any record failure
// rolls back and keeps the hold. Lock order (v16 p2): the row lock is held
// before the records file lock is taken, never the other way round.
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
		return usageError{fmt.Errorf("mission release: %s's held run (%s) was never swept; pass --ack-unswept after removing its run roots (docs/m1-runbook.md); nothing released", m, orUnknown(class))}
	}
	var gen int64
	var released bool
	err = crashpoint.Around("release.commit", func() error {
		var err error
		released, err = st.ReleaseOperatorHold(ctx, m, func(g int64) error {
			gen = g
			return crashpoint.Around("release.record", func() error {
				return records.Append(recordsDir, records.Record{Mission: m, Attempt: attempt, Kind: records.ReleaseAuthorized,
					Class: releaseClass, Generation: g, Reason: reason, AckUnswept: ackUnswept})
			})
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("mission release: hold kept: %w", err)
	}
	if !released {
		return fmt.Errorf("mission release: %s's hold was released by someone else", m)
	}
	fmt.Fprintf(os.Stderr, "fab: released the operator hold on %s (attempt %s, generation %d): %s\n", m, attempt, gen, reason)
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
