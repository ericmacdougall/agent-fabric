package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fabric/internal/crashpoint"
	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/records"
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
// visits every binding of this host without its settled marker, whatever
// its attempt's state (crash-point finding F2), except an attempt that is
// still live (LEASED, PREPARING, RUNNING, or UNKNOWN until reconcile
// quarantines it):
//   - never launched (no ATTEMPT_ACKED, or finished abandoned in
//     prelaunch): nothing ran, so the unstarted settle runs at once;
//   - otherwise restart recovery as for a quarantined attempt: a verified
//     restart stop settles it, anything else keeps it unsettled and holds.
//
// Each attempt is handled at most once per fabd process.
func (s *Scheduler) recoverQuarantined(ctx context.Context) int {
	c := s.cfg.Credential
	if c == nil {
		return 0
	}
	ents, err := os.ReadDir(c.BindingsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabd: binding recovery: bindings directory unreadable:", err)
		return 0
	}
	n := 0
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.Name() == "retained.json" {
			continue
		}
		a, err := domain.ParseAttemptID(name)
		if err != nil {
			continue
		}
		s.mu.Lock()
		done := s.recovered[a]
		s.mu.Unlock()
		if done {
			continue
		}
		if u, err := credential.Unsettled(c.BindingsDir, string(a)); err == nil && !u {
			continue
		}
		var mission domain.MissionID
		var fence int64
		var started time.Time
		var state string
		var launched bool
		err = s.st.Pool().QueryRow(ctx, `SELECT a.mission_id, a.fence, a.started_at, a.state,
			EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_ACKED' AND e.payload->>'attempt' = a.id)
			AND NOT EXISTS (SELECT 1 FROM events e WHERE e.mission_id = a.mission_id AND e.kind = 'ATTEMPT_FINISHED'
				AND e.payload->>'attempt' = a.id AND COALESCE((e.payload->>'prelaunch')::boolean, false))
			FROM attempts a WHERE a.id = $1 AND a.host = $2`, string(a), string(s.cfg.Host)).Scan(&mission, &fence, &started, &state, &launched)
		if err != nil {
			continue // another host's attempt, or no attempt row: not ours to settle
		}
		switch domain.AttemptState(state) {
		case domain.AttemptLeased, domain.AttemptPreparing, domain.AttemptRunning, domain.AttemptUnknown:
			continue // live, or awaiting reconcile
		}
		if !launched {
			s.settleUnstarted(a)
			if u, err := credential.Unsettled(c.BindingsDir, string(a)); err != nil || u {
				continue // retried on the next tick
			}
			fmt.Fprintf(os.Stderr, "fabd: settled %s, which never launched (%s)\n", a, state)
		} else if !s.recoverOne(ctx, mission, a, fence, started) {
			continue // the hold was not written: retried on the next tick
		}
		s.mu.Lock()
		if s.recovered == nil {
			s.recovered = map[domain.AttemptID]bool{}
		}
		s.recovered[a] = true
		s.mu.Unlock()
		n++
	}
	return n
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
