package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/store"
	"fabric/internal/tools"
)

// ErrStartRefused: the native start rule refused the grant (goodqa#15087).
// Nothing was written and no key command ran.
var ErrStartRefused = errors.New("native start refused")

// The worktree binding (goodqa#15283). Each refusal also wraps
// ErrStartRefused and, like it, comes before SelfTest, oracle hooks, the key
// command, the flock and any write.
var (
	// ErrWorktreeMismatch: the attempt is bound to another canonical
	// worktree than the grant's. The message names both.
	ErrWorktreeMismatch = errors.New("attempt worktree mismatch")
	// ErrWorktreeUnbound: the attempt has no recorded worktree (it started
	// before migration 0038). It is never adopted.
	ErrWorktreeUnbound = errors.New("attempt has no recorded worktree")
	// ErrWorktreeMoved: during a run, the recorded worktree no longer
	// resolves to the directory it named at start; the next tool is refused.
	ErrWorktreeMoved = tools.ErrWorktreeMoved
)

// canonicalWorktree resolves the grant's worktree (symlinks evaluated, then
// cleaned), requires a directory that passes domain.ValidWorktree, and pins
// its (dev, ino) for this run. It only reads the filesystem.
func canonicalWorktree(p string) (*tools.WorktreePin, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, refuse("worktree %q does not resolve: %v", p, err)
	}
	real = filepath.Clean(real)
	if err := domain.ValidWorktree(real); err != nil {
		return nil, refuse("worktree %q: %v", p, err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return nil, refuse("worktree %q (resolved %q) is not a directory", p, real)
	}
	pin, err := tools.PinWorktree(real)
	if err != nil {
		return nil, refuse("worktree %q: %v", p, err)
	}
	return pin, nil
}

// sameWorktree is the binding's comparison: canonical paths, byte for byte,
// with no case folding (NEG-6).
func sameWorktree(stored, canonical string) bool { return stored == canonical }

// startCase is what the native start rule decided for a grant.
type startCase int

const (
	// caseNew: no attempt and no mission, or an IDLE anthropic-api mission:
	// admit, then start.
	caseNew startCase = iota
	// caseStartAdmitted: no attempt, the anthropic-api mission is already
	// ADMITTED with no live or unresolved attempt ("awaiting mission run
	// --grant"): start without a second admit.
	caseStartAdmitted
	// caseReplay: the attempt has a recorded outcome: replay it, no request.
	caseReplay
	// caseResume: the attempt is RUNNING on this host with the grant's
	// fence, a live lease and no outcome: a crash resume.
	caseResume
)

type startPlan struct {
	c        startCase
	fence    int64  // caseReplay, caseResume
	lockPath string // caseResume: the stored path, never recomputed
	outcome  string // caseReplay
	note     string // caseReplay
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrStartRefused, fmt.Sprintf(format, a...))
}

// planStart is the native start rule (goodqa#15087 Scope, H1, H3, V1). It
// only reads, so a refusal comes before SelfTest, the key command and any
// write. worktree is the grant's canonical worktree (canonicalWorktree).
func planStart(ctx context.Context, st *store.Store, g Grant, worktree string) (startPlan, error) {
	// goodqa#15324 S1b: a ladder mission holds receipts, never work. Refused
	// before SelfTest, the key command and any write, row or no row.
	if domain.IsLadderMission(domain.MissionID(g.Mission)) {
		return startPlan{}, refuse("mission %s is a ladder mission; it holds receipts and is never started", g.Mission)
	}
	var (
		mission, state, path, lockPath, host string
		fence                                int64
		live                                 bool
		runFence                             *int64
		outcome, note, stored                *string
	)
	err := st.Pool().QueryRow(ctx, `SELECT a.mission_id, a.state, a.fence, a.path, coalesce(a.lock_path, ''), coalesce(a.host, ''),
		(a.lease_until IS NOT NULL AND a.lease_until >= now()), r.fence, r.outcome, r.outcome_note, a.worktree
		FROM attempts a LEFT JOIN agent_runs r ON r.attempt_id = a.id WHERE a.id = $1`, g.Attempt).
		Scan(&mission, &state, &fence, &path, &lockPath, &host, &live, &runFence, &outcome, &note, &stored)
	switch {
	case err == nil:
		return planExisting(g, worktree, stored, mission, state, path, lockPath, host, fence, live, runFence, outcome, note)
	case !errors.Is(err, pgx.ErrNoRows):
		return startPlan{}, err
	}
	var provider, execution, terminal string
	var busy bool
	err = st.Pool().QueryRow(ctx, `SELECT provider, execution, terminal,
		EXISTS (SELECT 1 FROM attempts a WHERE a.mission_id = m.id
		        AND a.state IN ('ADMITTED','LEASED','PREPARING','RUNNING','SUCCEEDED_UNVERIFIED','UNKNOWN','RECONCILING'))
		FROM missions m WHERE m.id = $1`, g.Mission).Scan(&provider, &execution, &terminal, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return startPlan{c: caseNew}, nil
	}
	if err != nil {
		return startPlan{}, err
	}
	switch {
	case domain.Provider(provider) != domain.ProviderAnthropicAPI && !skipPrecheck:
		return startPlan{}, refuse("mission %s is %s; the native runner runs anthropic-api missions only", g.Mission, provider)
	case terminal != string(domain.TermNone):
		return startPlan{}, refuse("mission %s has ended (%s)", g.Mission, terminal)
	case execution == string(domain.ExecIdle):
		return startPlan{c: caseNew}, nil
	case execution == string(domain.ExecAdmitted) && !busy:
		return startPlan{c: caseStartAdmitted}, nil
	}
	return startPlan{}, refuse("mission %s is %s; a new native attempt needs it IDLE, or ADMITTED with no live attempt", g.Mission, execution)
}

func planExisting(g Grant, worktree string, stored *string, mission, state, path, lockPath, host string, fence int64, live bool, runFence *int64, outcome, note *string) (startPlan, error) {
	switch {
	case mission != g.Mission:
		return startPlan{}, refuse("attempt %s belongs to mission %s", g.Attempt, mission)
	case domain.Provider(path) != domain.ProviderAnthropicAPI:
		return startPlan{}, refuse("attempt %s was started on path %s", g.Attempt, path)
	}
	// goodqa#15283: the worktree binding, before any state rule, so a resume
	// and a replay are both covered.
	if err := checkBinding(g, worktree, stored); err != nil {
		return startPlan{}, err
	}
	// H3: the state first. An attempt that reconcile owns is refused even
	// with a recorded outcome; a replay would only meet a reducer refusal.
	switch domain.AttemptState(state) {
	case domain.AttemptUnknown, domain.AttemptQuarantined, domain.AttemptReconciling:
		return startPlan{}, refuse("attempt %s is %s; reconcile and the operator handle it", g.Attempt, state)
	}
	// H1: the run row must belong to the attempt's current fence.
	if runFence != nil && *runFence != fence {
		return startPlan{}, refuse("attempt %s holds fence %d but its run was recorded under fence %d", g.Attempt, fence, *runFence)
	}
	if outcome != nil {
		n := ""
		if note != nil {
			n = *note
		}
		return startPlan{c: caseReplay, fence: fence, outcome: *outcome, note: n}, nil
	}
	switch {
	case domain.AttemptState(state) != domain.AttemptRunning:
		return startPlan{}, refuse("attempt %s is %s with no recorded outcome", g.Attempt, state)
	case !live:
		// V1: an expired lease is not revived; reconcile settles it.
		return startPlan{}, refuse("attempt %s has an expired lease", g.Attempt)
	case lockPath == "":
		return startPlan{}, refuse("attempt %s has no lock path (started before goodqa#15087)", g.Attempt)
	case domain.HostID(host) != hostid.Default():
		// Owner assumption A4: the flock is host-local, so only the host the
		// attempt runs on may resume it.
		return startPlan{}, refuse("attempt %s runs on %s, not %s", g.Attempt, host, hostid.Default())
	}
	return startPlan{c: caseResume, fence: fence, lockPath: lockPath}, nil
}

// checkBinding is the worktree binding (goodqa#15283): a stored NULL is
// ErrWorktreeUnbound; a stored path other than the grant's canonical
// worktree is ErrWorktreeMismatch, naming both.
func checkBinding(g Grant, worktree string, stored *string) error {
	switch {
	case stored == nil:
		return fmt.Errorf("%w: %w: attempt %s has no recorded worktree (it started before migration 0038), so it cannot be resumed or replayed from %q; reconcile attempt %s and start a new attempt",
			ErrStartRefused, ErrWorktreeUnbound, g.Attempt, worktree, g.Attempt)
	case !sameWorktree(*stored, worktree):
		return fmt.Errorf("%w: %w: attempt %s is bound to worktree %q, but this grant's worktree %q resolves to %q; run the grant from %q, or reconcile attempt %s and start a new attempt for %q",
			ErrStartRefused, ErrWorktreeMismatch, g.Attempt, *stored, g.Worktree, worktree, *stored, g.Attempt, worktree)
	}
	return nil
}

// recheckWorktree repeats the binding check with the flock held (red-team
// K3). planStart runs before the flock, so two new runs of one attempt id
// from different worktrees can both see no row; the one that loses the race
// finds the winner's row here and is refused before the key command and any
// write. No row yet means this run is still the one that starts it.
func recheckWorktree(ctx context.Context, st *store.Store, g Grant, worktree string) error {
	var stored *string
	err := st.Pool().QueryRow(ctx, `SELECT worktree FROM attempts WHERE id = $1`, g.Attempt).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return checkBinding(g, worktree, stored)
}

// beforeLocks, when set by a test, runs after the start rule and SelfTest and
// before the flock is taken (red-team K3). Always nil in production.
var beforeLocks func()

// heartbeatFault, when set by a test, replaces one heartbeat's result.
var heartbeatFault func() error

// skipPrecheck, set only by TestNativePathDefenseInDepth, turns off the
// native runner's own provider checks (the start rule and the catalog
// ensure), so the test proves the reducer and the database still refuse a
// native start on a claude-code mission. Always false in production.
var skipPrecheck bool

// heartbeat keeps the attempt's lease alive every Lease/3 while the loop
// runs (goodqa#15087). A refused heartbeat (stale fence, attempt no longer
// live, not found) or no successful heartbeat for longer than Lease minus
// Lease/3 cancels the loop's context with the cause, so the loop stops
// before its next provider request and nothing is settled. stop ends it.
func heartbeat(ctx context.Context, st *store.Store, attempt domain.AttemptID, fence int64, cancel context.CancelCauseFunc) (stop func()) {
	lease := st.Lease
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	every := lease / 3
	lapse := lease - every
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			var err error
			if heartbeatFault != nil {
				err = heartbeatFault()
			}
			if err == nil {
				err = st.HeartbeatLive(ctx, attempt, fence)
			}
			switch {
			case err == nil:
				last = time.Now()
			case errors.Is(err, domain.ErrStaleFence), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrNotFound):
				cancel(fmt.Errorf("native heartbeat refused, loop stopped, nothing settled: %w", err))
				return
			case time.Since(last) > lapse:
				cancel(fmt.Errorf("native heartbeat lapsed (no success for %s), loop stopped, nothing settled: %w", time.Since(last).Round(time.Millisecond), err))
				return
			}
		}
	}()
	return func() { close(done); <-finished }
}

// ensureLocks takes the attempt's flock on path, then probes the agent-run
// DB lock (V3 order); if the DB lock is held, it releases the flock. The
// loop takes the DB lock itself; the flock, held until the process exits,
// is what keeps a second native process out between the two.
func ensureLocks(ctx context.Context, db *agent.DB, attempt domain.AttemptID, path string) (release func(), err error) {
	f, err := acquireLock(attempt, path)
	if err != nil {
		return nil, err
	}
	unlock, err := db.Lock(ctx, attempt)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("attempt busy: the agent-run lock of %s: %w", attempt, err)
	}
	unlock()
	return func() { f.Close() }, nil
}
