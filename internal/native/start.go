package native

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/agent"
	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/store"
)

// ErrStartRefused: the native start rule refused the grant (goodqa#15087).
// Nothing was written and no key command ran.
var ErrStartRefused = errors.New("native start refused")

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
// write.
func planStart(ctx context.Context, st *store.Store, g Grant) (startPlan, error) {
	var (
		mission, state, path, lockPath, host string
		fence                                int64
		live                                 bool
		runFence                             *int64
		outcome, note                        *string
	)
	err := st.Pool().QueryRow(ctx, `SELECT a.mission_id, a.state, a.fence, a.path, coalesce(a.lock_path, ''), coalesce(a.host, ''),
		(a.lease_until IS NOT NULL AND a.lease_until >= now()), r.fence, r.outcome, r.outcome_note
		FROM attempts a LEFT JOIN agent_runs r ON r.attempt_id = a.id WHERE a.id = $1`, g.Attempt).
		Scan(&mission, &state, &fence, &path, &lockPath, &host, &live, &runFence, &outcome, &note)
	switch {
	case err == nil:
		return planExisting(g, mission, state, path, lockPath, host, fence, live, runFence, outcome, note)
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
	case domain.Provider(provider) != domain.ProviderAnthropicAPI:
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

func planExisting(g Grant, mission, state, path, lockPath, host string, fence int64, live bool, runFence *int64, outcome, note *string) (startPlan, error) {
	switch {
	case mission != g.Mission:
		return startPlan{}, refuse("attempt %s belongs to mission %s", g.Attempt, mission)
	case domain.Provider(path) != domain.ProviderAnthropicAPI:
		return startPlan{}, refuse("attempt %s was started on path %s", g.Attempt, path)
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

// heartbeatFault, when set by a test, replaces one heartbeat's result.
var heartbeatFault func() error

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
				err = st.Heartbeat(ctx, attempt, fence)
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
