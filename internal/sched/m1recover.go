package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// recoverQuarantined runs on the token path after reconcileUnknown. Each
// quarantined attempt of this host that holds a binding without its settled
// marker is handled at most once per fabd process.
func (s *Scheduler) recoverQuarantined(ctx context.Context) int {
	c := s.cfg.Credential
	if c == nil {
		return 0
	}
	rows, err := s.st.Pool().Query(ctx, `SELECT id, mission_id, fence, started_at FROM attempts
		WHERE state = 'QUARANTINED' AND host = $1 ORDER BY started_at LIMIT 20`, string(s.cfg.Host))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabd: quarantine recovery:", err)
		return 0
	}
	type q struct {
		id      domain.AttemptID
		mission domain.MissionID
		fence   int64
		started time.Time
	}
	var page []q
	for rows.Next() {
		var x q
		if err := rows.Scan(&x.id, &x.mission, &x.fence, &x.started); err != nil {
			rows.Close()
			return 0
		}
		page = append(page, x)
	}
	rows.Close()
	n := 0
	for _, x := range page {
		s.mu.Lock()
		done := s.recovered[x.id]
		s.mu.Unlock()
		if done {
			continue
		}
		if u, err := credential.Unsettled(c.BindingsDir, string(x.id)); err == nil && !u {
			continue // no binding (not a token launch), or already settled
		}
		if !s.recoverOne(ctx, x.mission, x.id, x.fence, x.started) {
			continue // the hold was not written: retried on the next tick
		}
		s.mu.Lock()
		if s.recovered == nil {
			s.recovered = map[domain.AttemptID]bool{}
		}
		s.recovered[x.id] = true
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
