package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fabric/internal/credential"
	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/provider/claude"
	"fabric/internal/records"
)

// goodqa#15966 AC-15 to AC-18, AC-29 (owner rulings C and D): the one settle
// sequence for an attempt launched with the subscription token. Every exit
// path, a panic included, ends with MarkSettled and then GC, so a raw token
// version is never kept past the attempt's settle.
//
//  1. LoadBound, redact the raw streams into the fabd-only redacted copy,
//     gate (credential.Policy.SettleSubmit semantics); only on a clean
//     copy is the finish decided and the pre-submit effect step run, both
//     from the redacted copy, never the raw stream.
//  2. The AC-16 sweep (sweep.Fabd with credential.BuildSweepInput), which
//     launches only on a Verified stop receipt.
//  3. The sweep record (counts only) in the stream directory.
//  4. records.Append for credential_exposure and sweep_incomplete (and the
//     delivery_blocked record of a fail-closed settle).
//  5. SaveMaterial when the sweep is incomplete.
//  6. Raw stream removal, only after a clean, unblocked redaction.
//  7. MarkSettled, deferred: every path.
//  8. GC, after MarkSettled.

// Settle steps, the names the fault-injection seam sees.
const (
	stepLoad      = "load"
	stepRedact    = "redact"
	stepSweep     = "sweep"
	stepSweepRec  = "sweep_record"
	stepRecords   = "records"
	stepMaterial  = "material"
	stepRawRemove = "raw_remove"
	stepMark      = "mark"
	stepGC        = "gc"
)

// Fail-closed classes that hold the mission for an operator look (owner
// ruling C): no further attempt starts until the operator releases it.
var holdClasses = map[string]bool{
	credential.ClassMaterialUnavailable: true,
	credential.ClassRotatedMidAttempt:   true,
	credential.ClassNumericHit:          true,
	credential.ClassBrokeStream:         true,
}

// Classes this file adds.
const (
	classRedactionFailed = "redaction_failed"
	classSettlePanic     = "settle_panic"
	reasonNoLauncher     = "no_launcher"
	reasonInputError     = "input_error"
)

// File names in an attempt's stream directory ({run area}/streams/{attempt}).
const (
	redactedDirName = "redacted"
	redactedDone    = "redacted.done" // counts of a finished redaction (no content)
	sweepRecordName = "sweep.json"
)

// m1Settled is what the sequence decided.
type m1Settled struct {
	Cmd     *domain.Command // nil: no finish (lease expiry, as today)
	Hold    bool            // hold the mission after the finish
	Blocked string          // the delivery_blocked class of a fail-closed settle, or ""
	Exposed bool
	Sweep   sweep.Report
}

// redactSummary is redacted.done: enough to reuse a finished redaction after
// a crash between the copy and MarkSettled, never any stream byte.
type redactSummary struct {
	Counts  map[string]int `json:"counts"`
	Blocked string         `json:"blocked,omitempty"`
}

func (s *Scheduler) settleFail(step string) error {
	if s.settleFault != nil {
		return s.settleFault(step)
	}
	return nil
}

// sweepRoots are the worker-writable roots of the run (AC-16): its
// CLAUDE_CONFIG_DIR, per-run HOME and private TMPDIR.
func sweepRoots(runDir string) []string {
	return []string{filepath.Join(runDir, "claude-config"), filepath.Join(runDir, "home"), filepath.Join(runDir, "tmp")}
}

// settleM1 runs the sequence. rc is the verified stop receipt of the exited
// worker, or nil when none was taken (the sweep is then incomplete).
func (s *Scheduler) settleM1(mission domain.MissionID, attempt domain.AttemptID, fence int64, sdir, runDir string, code int,
	rc *host.StopReceipt, effect func(cmd *domain.Command, redacted string, b *credential.Bound)) (out m1Settled) {
	c := s.cfg.Credential
	rec := func(k records.Kind, class string) {
		if s.cfg.RecordsDir == "" {
			return
		}
		if err := s.settleFail(stepRecords); err == nil {
			err = records.Append(s.cfg.RecordsDir, records.Record{Mission: mission, Attempt: attempt, Kind: k, Class: class})
			if err == nil {
				return
			}
		}
		fmt.Fprintf(os.Stderr, "fabd: m1 record %s for %s not written\n", k, attempt)
	}
	block := func(class string) {
		out.Blocked = class
		out.Hold = holdClasses[class]
		out.Cmd = &domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: attempt, Fence: fence,
			Outcome: "abandoned", Reason: "m1:delivery_blocked:" + class}
		rec(records.DeliveryBlocked, class)
	}
	defer func() {
		if p := recover(); p != nil {
			// A panic settles nothing: no finish (the lease expires into
			// UNKNOWN for reconcile), and the raw files stay. No hold: a
			// hold over a leased attempt would exempt it from lease expiry
			// (review M2).
			fmt.Fprintf(os.Stderr, "fabd: settle of %s panicked; failing closed\n", attempt)
			out = m1Settled{Blocked: classSettlePanic}
			rec(records.DeliveryBlocked, classSettlePanic)
		}
		if err := s.settleFail(stepMark); err == nil {
			err = credential.MarkSettled(c.BindingsDir, string(attempt))
			if err != nil {
				fmt.Fprintf(os.Stderr, "fabd: mark %s settled: %v\n", attempt, err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "fabd: mark %s settled: %v\n", attempt, err)
		}
		if err := s.settleFail(stepGC); err == nil {
			if _, err := c.Policy.GC(c.BindingsDir); err != nil {
				fmt.Fprintf(os.Stderr, "fabd: credential gc after %s: %v\n", attempt, err)
			}
		}
	}()

	// 1. Bound values, redaction and the gate.
	var b *credential.Bound
	err := s.settleFail(stepLoad)
	if err == nil {
		var others []credential.OtherCredential
		if c.Others != nil {
			others = c.Others()
		}
		b, err = c.Policy.LoadBound(c.BindingsDir, string(attempt), c.IDKey, others)
	}
	if err != nil {
		// No bound values: no redaction, no sweep, raw files kept.
		class := credential.ClassMaterialUnavailable
		var se *credential.SettleError
		if errors.As(err, &se) {
			class = se.Class
		}
		block(class)
		return out
	}
	red := filepath.Join(sdir, redactedDirName)
	sum, rerr := s.redactOnce(b, sdir, red)
	clean := false
	switch {
	case rerr != nil:
		block(classRedactionFailed)
	case sum.Blocked != "":
		block(sum.Blocked)
	default:
		cmd, err := settleCommand(s.settler(), mission, attempt, fence, red, code)
		if err != nil {
			block(classRedactionFailed)
		} else {
			clean = true
			if cmd != nil {
				if effect != nil {
					effect(cmd, red, b)
				}
				out.Cmd = cmd
			}
		}
	}
	for l, n := range sum.Counts {
		if n > 0 && l != credential.LabelLiteral {
			out.Exposed = true
		}
	}

	// 2. The sweep.
	input, ierr := credential.BuildSweepInput(b)
	switch {
	case ierr != nil:
		out.Sweep = incompleteReport(reasonInputError)
	case s.settleFail(stepSweep) != nil:
		out.Sweep = incompleteReport(sweep.ReasonLaunch)
	case s.cfg.Sweep == nil:
		out.Sweep = incompleteReport(reasonNoLauncher)
	default:
		var stop host.StopReceipt
		if rc != nil {
			stop = *rc
		}
		out.Sweep = sweep.Fabd(stop, s.cfg.FabBin, sweepRoots(runDir), input, s.cfg.Sweep)
	}
	if out.Sweep.Exposed() {
		out.Exposed = true
	}
	// 3. The sweep record: counts only.
	if err := s.settleFail(stepSweepRec); err == nil {
		data, _ := json.Marshal(out.Sweep)
		err = writeFileAtomic(filepath.Join(sdir, sweepRecordName), data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fabd: sweep record for %s not written\n", attempt)
		}
	}
	// 4. Records.
	if out.Exposed {
		rec(records.CredentialExposure, "")
	}
	if !out.Sweep.Complete {
		rec(records.SweepIncomplete, "")
		// 5. Keep the sweep's matching material (keys and fingerprints, no
		// value) for a later re-sweep of the kept run roots.
		if input != nil {
			err := s.settleFail(stepMaterial)
			if err == nil {
				err = credential.SaveMaterial(c.BindingsDir, string(attempt), input)
			}
			if err != nil && !errors.Is(err, os.ErrExist) {
				fmt.Fprintf(os.Stderr, "fabd: sweep material for %s not saved: %v\n", attempt, err)
			}
		}
	}
	if out.Exposed && out.Cmd != nil {
		out.Cmd.Reason = joinReason("m1:credential_exposure", out.Cmd.Reason)
	}
	// 6. Raw stream removal, only after a clean, unblocked redaction.
	if clean {
		if err := s.settleFail(stepRawRemove); err == nil {
			for _, f := range []string{"stream.jsonl", "stderr.log"} {
				if err := os.Remove(filepath.Join(sdir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "fabd: raw %s of %s not removed: %v\n", f, attempt, err)
				}
			}
		}
	}
	return out
}

// redactOnce redacts the raw streams into red, or reuses a redaction that
// finished before a crash (redacted.done is written only after both copies
// are durable). A partial copy is discarded and redone from the raw files;
// a missing raw stream without a finished redaction fails.
func (s *Scheduler) redactOnce(b *credential.Bound, sdir, red string) (redactSummary, error) {
	var sum redactSummary
	if data, err := os.ReadFile(filepath.Join(sdir, redactedDone)); err == nil {
		if json.Unmarshal(data, &sum) == nil {
			return sum, nil
		}
		return sum, errors.New("redaction summary malformed")
	}
	if err := s.settleFail(stepRedact); err != nil {
		return sum, err
	}
	raw := filepath.Join(sdir, "stream.jsonl")
	if _, err := os.Lstat(raw); err != nil {
		return sum, fmt.Errorf("raw stream: %w", err)
	}
	if err := os.MkdirAll(red, 0o700); err != nil {
		return sum, err
	}
	for _, f := range []string{"stream.jsonl", "stderr.log"} {
		if err := os.Remove(filepath.Join(red, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sum, err
		}
	}
	res, err := b.RedactStreams(raw, filepath.Join(sdir, "stderr.log"), red)
	if err != nil {
		return sum, err
	}
	sum = redactSummary{Counts: res.Counts, Blocked: res.Blocked}
	data, _ := json.Marshal(sum)
	if err := writeFileAtomic(filepath.Join(sdir, redactedDone), data); err != nil {
		return sum, err
	}
	return sum, nil
}

func incompleteReport(reason string) sweep.Report {
	return sweep.Report{Reasons: []string{reason}, Entries: map[string]int{}, Counts: map[string]int{}}
}

func joinReason(prefix, reason string) string {
	if reason == "" {
		return prefix
	}
	return prefix + " " + reason
}

// writeFileAtomic writes data 0600 under a temporary name, fsyncs it and
// renames it into place.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// finishM1 sends the settle's finish, if any, and then holds the mission
// when the settle failed closed for an operator look (owner ruling C) or the
// token was refused (AC-19). A held mission admits no further attempt
// until the operator releases it.
func (s *Scheduler) finishM1(ctx context.Context, mission domain.MissionID, out m1Settled) error {
	if out.Cmd != nil {
		if err := s.finishRetry(ctx, *out.Cmd); err != nil {
			return err
		}
	}
	if out.Cmd == nil {
		// Nothing finished: the attempt is still leased, and a hold would
		// exempt it from lease expiry and reconcile (review M2).
		return nil
	}
	refused := out.Cmd != nil && out.Cmd.Outcome == "abandoned" && strings.Contains(out.Cmd.Reason, claude.ReasonCredentialRejected)
	if !out.Hold && !refused {
		return nil
	}
	if err := s.st.HoldMission(ctx, s.st.Pool(), mission); err != nil {
		return fmt.Errorf("hold after a fail-closed settle: %w", err)
	}
	why := "settle failed closed (" + out.Blocked + ")"
	if refused {
		why = "the subscription token was refused"
	}
	fmt.Fprintf(os.Stderr, "fabd: held %s: %s\n", mission, why)
	return nil
}

// settleUnstarted settles a binding whose worker never started (a refusal
// after the binding record and before the start): nothing ran, so GC may
// drop its token version at once.
func (s *Scheduler) settleUnstarted(attempt domain.AttemptID) {
	c := s.cfg.Credential
	if err := credential.MarkSettled(c.BindingsDir, string(attempt)); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: mark unstarted %s settled: %v\n", attempt, err)
		return
	}
	if _, err := c.Policy.GC(c.BindingsDir); err != nil {
		fmt.Fprintf(os.Stderr, "fabd: credential gc after unstarted %s: %v\n", attempt, err)
	}
}
