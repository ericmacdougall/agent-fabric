package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fabric/internal/crashpoint"
	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/records"
	"fabric/internal/store"
)

// goodqa#15966 AC-35(i), erratum E9: restart recovery of a token-path
// attempt the prober quarantined. After a fabd restart no in-memory stop
// handle survives, so Config.VerifyGone stands in for W1's verified Stop.
//   - Verified: the same settle sequence as a live exit (redact, gate,
//     sweep, records, MarkSettled, GC). The attempt is already terminal, so
//     there is no effect step and no FINISH; a fail-closed class, or a
//     refused token, places the AC-34 hold.
//   - Otherwise the attempt stays quarantined and unsettled: sweep_incomplete
//     (restart_unverified) is recorded, the sweep material is kept, and the
//     AC-34 hold is placed. Nothing is swept, removed or finished.

// classRestartUnverified is the sweep_incomplete and hold class of a
// quarantined attempt whose runner could not be verified gone.
const classRestartUnverified = "restart_unverified"

// recoverQuarantined runs on the token path after reconcileUnknown. It
// visits the union of every binding of this host without its settled
// marker (crash-point finding F2) and every attempt of this host whose
// START carried the M1 flag and has no settled marker (F3), whatever its
// state, except an attempt that is still live (LEASED, PREPARING, RUNNING,
// or UNKNOWN until reconcile quarantines it). A START without the flag and
// without a binding (pre-M1) is never visited.
//   - launched (ATTEMPT_ACKED, no prelaunch FINISH) with no binding: its
//     bound material is lost. sweep_incomplete (material_unavailable) is
//     recorded and the credential_material_unavailable hold placed; only
//     after the hold commits is the attempt marked settled;
//   - never launched, binding or not: nothing ran, so the unstarted settle
//     runs at once;
//   - launched with a binding: restart recovery as for a quarantined
//     attempt: a verified restart stop settles it, anything else keeps it
//     unsettled and holds.
//
// Two or more lost bindings in one scan, or a missing bindings directory,
// mean the directory was lost or emptied: one bindings_lost record and one
// stderr ALERT line for that scan (docs/m1-runbook.md#bindings-lost). It
// never exits. Each attempt is handled at most once per fabd process.
func (s *Scheduler) recoverQuarantined(ctx context.Context) int {
	c := s.cfg.Credential
	if c == nil {
		return 0
	}
	dirMissing := false
	ents, err := os.ReadDir(c.BindingsDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fabd's run area start creates it; a missing one was lost while
		// fabd ran. Recreate it (0700, as at start) so settled markers can
		// be written again, and alert below.
		dirMissing = true
		if err := crashpoint.Around("recovery.bindings.mkdir", func() error { return os.Mkdir(c.BindingsDir, 0o700) }); err != nil && !errors.Is(err, os.ErrExist) {
			fmt.Fprintln(os.Stderr, "fabd: binding recovery: bindings directory missing and not recreated:", err)
		}
	case err != nil:
		fmt.Fprintln(os.Stderr, "fabd: binding recovery: bindings directory unreadable:", err)
		return 0
	}
	fromDir := map[domain.AttemptID]bool{}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.Name() == "retained.json" {
			continue
		}
		if a, err := domain.ParseAttemptID(name); err == nil {
			fromDir[a] = true
		}
	}
	flagged := map[domain.AttemptID]store.M1Start{}
	if rows, err := s.st.M1Starts(ctx, s.cfg.Host, ""); err != nil {
		fmt.Fprintln(os.Stderr, "fabd: binding recovery: M1 starts unreadable:", err)
	} else {
		for _, r := range rows {
			flagged[r.Attempt] = r
		}
	}
	ids := make([]domain.AttemptID, 0, len(fromDir)+len(flagged))
	for a := range fromDir {
		ids = append(ids, a)
	}
	for a := range flagged {
		if !fromDir[a] {
			ids = append(ids, a)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	n := 0
	var lostFirst store.M1Start
	lostAttempts, lostMissions := 0, map[domain.MissionID]bool{}
	for _, a := range ids {
		s.mu.Lock()
		done := s.recovered[a]
		s.mu.Unlock()
		if done {
			continue
		}
		hasBinding, settled := bindingState(c.BindingsDir, a)
		if settled {
			continue
		}
		r, ok := flagged[a]
		if !ok {
			// From the bindings directory only (a START from before the
			// flag): ids holds nothing else, so an unflagged START with no
			// binding never gets here.
			var state string
			err := s.st.Pool().QueryRow(ctx, `SELECT a.mission_id, a.fence, a.started_at, a.state,
				EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_ACKED' AND e.payload->>'attempt' = a.id)
				AND NOT EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_FINISHED'
					AND e.payload->>'attempt' = a.id AND COALESCE((e.payload->>'prelaunch')::boolean, false))
				FROM attempts a WHERE a.id = $1 AND a.host = $2`, string(a), string(s.cfg.Host)).Scan(&r.Mission, &r.Fence, &r.Started, &state, &r.Launched)
			if err != nil {
				continue // another host's attempt, or no attempt row: not ours to settle
			}
			r.Attempt, r.State = a, domain.AttemptState(state)
		}
		switch r.State {
		case domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning, domain.AttemptUnknown:
			continue // live, or awaiting reconcile
		}
		switch {
		case !hasBinding && r.Launched:
			lostAttempts++
			if !s.recoverLost(ctx, r) {
				continue // retried on the next tick
			}
			if lostFirst.Attempt == "" {
				lostFirst = r
			}
			lostMissions[r.Mission] = true
		case !r.Launched:
			s.settleUnstarted(a)
			if _, settled := bindingState(c.BindingsDir, a); !settled {
				continue // retried on the next tick
			}
			fmt.Fprintf(os.Stderr, "fabd: settled %s, which never launched (%s)\n", a, r.State)
		default:
			if !s.recoverOne(ctx, r.Mission, a, r.Fence, r.Started) {
				continue // the hold was not written: retried on the next tick
			}
		}
		s.mu.Lock()
		if s.recovered == nil {
			s.recovered = map[domain.AttemptID]bool{}
		}
		s.recovered[a] = true
		s.mu.Unlock()
		n++
	}
	if dirMissing || lostAttempts >= 2 {
		s.alertBindingsLost(lostFirst, lostAttempts, len(lostMissions))
	}
	return n
}

// bindingState reports whether attempt has a binding record and a settled
// marker. Lstat, so a planted link counts as present; an error other than
// absence counts as a binding present and no settled marker (fail closed).
func bindingState(dir string, attempt domain.AttemptID) (hasBinding, settled bool) {
	_, err := os.Lstat(filepath.Join(dir, string(attempt)+".json"))
	hasBinding = !errors.Is(err, os.ErrNotExist)
	_, err = os.Lstat(filepath.Join(dir, string(attempt)+".settled"))
	return hasBinding, err == nil
}

// recoverLost holds an M1 attempt that launched and whose binding record is
// lost (F3): its bound values cannot be loaded, so nothing can be swept.
// The record and the hold commit before the settled marker; it reports
// whether the attempt is done with.
func (s *Scheduler) recoverLost(ctx context.Context, r store.M1Start) bool {
	c := s.cfg.Credential
	ok := true
	if s.cfg.RecordsDir != "" {
		if err := crashpoint.Around("recovery.lost.record", func() error {
			return records.Append(s.cfg.RecordsDir, records.Record{Mission: r.Mission, Attempt: r.Attempt, Kind: records.SweepIncomplete, Class: sweepMaterialUnavailable})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fabd: m1 record sweep_incomplete for %s not written\n", r.Attempt)
			ok = false // still held below; marked settled only once recorded
		}
	}
	if err := s.holdRecovered(ctx, r.Mission, r.Attempt, credential.ClassMaterialUnavailable); err != nil || !ok {
		return false
	}
	if err := crashpoint.Around("recovery.lost.mark", func() error { return credential.MarkSettled(c.BindingsDir, string(r.Attempt)) }); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: mark lost-binding %s settled: %v\n", r.Attempt, err)
		return false
	}
	if err := crashpoint.Around("recovery.lost.gc", func() error { _, err := c.Policy.GC(c.BindingsDir); return err }); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: credential gc after lost-binding %s: %v\n", r.Attempt, err)
	}
	fmt.Fprintf(os.Stderr, "fabd: held %s: binding record of %s lost (%s)\n", r.Mission, r.Attempt, credential.ClassMaterialUnavailable)
	return true
}

// bindingsLostAlert is the one stderr line of a scan that found the
// bindings directory lost or emptied.
const bindingsLostAlert = "fabd: ALERT bindings directory lost or emptied; %d M1 missions held; see docs/m1-runbook.md#bindings-lost\n"

// alertBindingsLost writes the bindings_lost record (counts only; once,
// since Append drops an identical record) and the stderr ALERT line. With
// nothing held there is no attempt to file the record under: the line
// alone is written.
func (s *Scheduler) alertBindingsLost(first store.M1Start, attempts, missions int) {
	if s.cfg.RecordsDir != "" && first.Attempt != "" && missions > 0 {
		if err := crashpoint.Around("recovery.bindings_lost.record", func() error {
			return records.Append(s.cfg.RecordsDir, records.Record{Mission: first.Mission, Attempt: first.Attempt, Kind: records.BindingsLost,
				Attempts: max(attempts, missions), Missions: missions})
		}); err != nil {
			fmt.Fprintln(os.Stderr, "fabd: bindings_lost record not written")
		}
	}
	fmt.Fprintf(os.Stderr, bindingsLostAlert, missions)
}

// recoverOne reports whether the attempt is done with: settled, or held.
func (s *Scheduler) recoverOne(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, started time.Time) bool {
	runDir := filepath.Join(s.cfg.WorkRoot, "run", string(attempt))
	sdir := s.streamDir(runDir, attempt)
	rc, why := s.verifyGone(ctx, attempt, started)
	if rc != nil {
		// The hold a settle owes commits before MarkSettled: if it fails the
		// binding stays unsettled, the next scan retries, and
		// prior_attempt_unsettled keeps blocking START meanwhile.
		held := false
		s.settleM1Mark(mission, attempt, fence, sdir, runDir, -1, rc, nil, func(out m1Settled) error {
			class := owedHold(out)
			if class == "" {
				return nil
			}
			if err := s.holdRecovered(ctx, mission, attempt, class); err != nil {
				return err
			}
			held = true
			return nil
		})
		if u, err := credential.Unsettled(s.cfg.Credential.BindingsDir, string(attempt)); err != nil || u {
			return false // retried on the next tick
		}
		fmt.Fprintf(os.Stderr, "fabd: quarantined %s settled after a verified restart stop (held=%v)\n", attempt, held)
		return true
	}
	fmt.Fprintf(os.Stderr, "fabd: quarantined %s not verified gone (%s); kept quarantined and unswept\n", attempt, why)
	c := s.cfg.Credential
	if s.cfg.RecordsDir != "" {
		if err := crashpoint.Around("recovery.unverified.record", func() error {
			return records.Append(s.cfg.RecordsDir, records.Record{Mission: mission, Attempt: attempt, Kind: records.SweepIncomplete, Class: classRestartUnverified})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fabd: m1 record sweep_incomplete for %s not written\n", attempt)
		}
	}
	var others []credential.OtherCredential
	if c.Others != nil {
		others = c.Others()
	}
	if b, err := c.Policy.LoadBound(c.BindingsDir, string(attempt), c.IDKey, others); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: bound values of %s not loadable; no sweep material saved\n", attempt)
	} else if input, err := credential.BuildSweepInput(b); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: sweep input of %s not built\n", attempt)
	} else if err := crashpoint.Around("recovery.material", func() error { return credential.SaveMaterial(c.BindingsDir, string(attempt), input) }); err != nil && !errors.Is(err, os.ErrExist) {
		fmt.Fprintf(os.Stderr, "fabd: sweep material for %s not saved: %v\n", attempt, err)
	}
	return s.holdRecovered(ctx, mission, attempt, classRestartUnverified) == nil
}

// verifyGone returns the receipt of a verified restart stop, or nil and why
// not. Another live attempt on this host is never verified (owner ruling
// E, point 4).
func (s *Scheduler) verifyGone(ctx context.Context, attempt domain.AttemptID, started time.Time) (*host.StopReceipt, string) {
	if s.cfg.VerifyGone == nil {
		return nil, "no verifier"
	}
	var live int
	if err := s.st.Pool().QueryRow(ctx, `SELECT count(*) FROM attempts WHERE host = $1 AND id <> $2
		AND state IN ('LEASED', 'PREPARING', 'RUNNING', 'UNKNOWN')`, string(s.cfg.Host), string(attempt)).Scan(&live); err != nil {
		return nil, "store unreadable"
	}
	if live > 0 {
		return nil, "another attempt is live on this host"
	}
	rc, err := s.cfg.VerifyGone(attempt, started)
	if err != nil || !rc.Verified || len(rc.Survivors) > 0 || len(rc.Escaped) > 0 {
		return nil, "stop not verified"
	}
	return &rc, ""
}

// holdRecovered places the AC-34 operator hold for a recovered attempt and
// its mission_hold record. The attempt is terminal, so no FINISH follows.
func (s *Scheduler) holdRecovered(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, class string) error {
	if err := s.settleFail(stepOpHold); err != nil {
		return err
	}
	if err := crashpoint.Around("recovery.hold", func() error { return s.st.OperatorHold(ctx, mission) }); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: operator hold for %s (%s) not written: %v\n", mission, class, err)
		return err
	}
	if s.cfg.RecordsDir != "" {
		if err := crashpoint.Around("recovery.hold.record", func() error {
			return records.Append(s.cfg.RecordsDir, records.Record{Mission: mission, Attempt: attempt, Kind: records.MissionHold, Class: class})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fabd: hold record for %s not written\n", mission)
		}
	}
	fmt.Fprintf(os.Stderr, "fabd: held %s after restart recovery of %s (%s)\n", mission, attempt, class)
	return nil
}
