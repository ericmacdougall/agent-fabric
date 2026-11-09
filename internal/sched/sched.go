// Package sched is the daemon loop (DESIGN.md 3.5, Appendix C): it consumes
// the outbox into the waits projection, expires leases, turns job exits into
// wakes, and launches a worker for every admitted mission that has no live
// attempt and no open wait. It never sleep-polls on the model's behalf: the
// loop blocks on a Postgres NOTIFY wake hint with a bounded backoff timer as
// the backstop (E6: notifies are hints, the tables are the record).
package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/envelope"
	"fabric/internal/host"
	"fabric/internal/projection"
	"fabric/internal/provider/claude"
	"fabric/internal/settle"
	"fabric/internal/store"
)

// WakeChannel is the NOTIFY channel every writer pings after a commit that
// may unblock the scheduler.
const WakeChannel = "fabric_wake"

// Launcher is how the scheduler starts and waits for one worker process.
type Launcher interface {
	Start(g host.Grant, argv []string, env []string, stdout, stderr *os.File) (host.Handle, error)
	Wait(h host.Handle) (int, error)
	// No StartJob: host jobs are disabled (goodqa#15082 F0). A job's argv
	// came from the confined run and was launched outside its sandbox.
}

// Stopper is the host Stop a Launcher must also provide when
// Config.ExitCleanup is on (the Darwin and Linux profiles both do).
type Stopper interface {
	Stop(h host.Handle, reason string, grace time.Duration) (host.StopReceipt, error)
}

// ErrHostJobsDisabled refuses a host job before anything is written or run.
// Typed, policy-approved job templates are future work (goodqa#15082).
var ErrHostJobsDisabled = fmt.Errorf("%w: host jobs are disabled (goodqa#15082)", domain.ErrInvalidTransition)

// Prober reads a runner back by attempt id (G1). known false: no answer.
type Prober interface {
	Alive(run domain.AttemptID) (alive, known bool)
	// JobAlive reads a host job's unit back (fab-job-<id>).
	JobAlive(job string) (alive, known bool)
}

// Config is the policy the scheduler applies. Model and effort come from the
// tier table upstream; here they are opaque strings copied into grants.
type Config struct {
	Host         domain.HostID
	Model        string
	Effort       string
	Rung         string
	TierTableSHA string
	Profile      string
	MaxTurns     int
	MaxBudgetUSD float64
	Wall         time.Duration
	WorkRoot     string // worktrees and run dirs live under here
	FabBin       string // the fab binary the daemon uses to watch jobs
	Launch       claude.Launch
	Backoff      time.Duration // backstop timer; doubles up to MaxBackoff
	MaxBackoff   time.Duration
	// Holds turns on the C5 dispatch gate (0012). Empty (the default) keeps
	// today's behaviour: no reservation, no grant. When set, each attempt
	// holds these items after START, and the runner starts only on a stored
	// GRANTED decision taken after ACK_START; holds are released at finish
	// and swept at expiry.
	Holds   []store.HoldItem
	HoldTTL time.Duration // default 2 x Wall; must lie in store.MinHoldTTL..MaxHoldTTL
	// RunnerDSN is the database a runner and its jobs connect with: a role
	// without the scheduler's privileges (0015). Required when gated.
	RunnerDSN string
	// ExitCleanup (FABRIC_WORKER_EXIT_CLEANUP=1 in fab daemon; default off)
	// stops and verifies a worker's descendants after its leader exits and
	// before anything is settled. A failed or unverified cleanup leaves the
	// attempt to lease expiry, never a terminal success. Off, a finished
	// worker settles exactly as before the flag existed.
	ExitCleanup bool
}

// Scheduler owns one host's dispatch loop.
type Scheduler struct {
	st  *store.Store
	cfg Config
	ln  Launcher
	// afterRead is a test barrier between reading the mission and starting
	// its attempt (owner_test.go); nil in production.
	afterRead func()
	// afterAck is a test cutpoint after ACK_START, before the dispatch grant.
	afterAck func()
	// beforeRun is a test cutpoint that can fail one mission's runOnce
	// (loop isolation, red-team D1); nil in production.
	beforeRun func(domain.MissionID) error
	// parked are missions a sweep step failed on, with when to try again
	// (red-team F1, D1): an unreadable log stays parked until restart.
	mu     sync.Mutex
	parked map[domain.MissionID]time.Time
	cfgErr error // gateErr at New; Tick refuses to run with it
	// runnerChecked: the RunnerDSN role passed CheckRunnerRole (G2).
	runnerMu      sync.Mutex
	runnerChecked bool
	prober        Prober
	lockProber    LockProber // native attempts' flock readback (goodqa#15087)
	// lockRequired: lease expiry and reconcile run only while hostLock is
	// held (fabd); lockWaitLogged keeps the wait to one log line.
	lockRequired   bool
	hostLock       *store.HostLock
	lockWaitLogged bool
	waitLogged     map[domain.MissionID]bool // capacity waits already named in the log
	// beforeHeartbeat is a test cutpoint that can fail one heartbeat (H2).
	beforeHeartbeat func() error
	// settleBackoff is the first wait between settle tries (goodqa#15082
	// T6); zero means the default.
	settleBackoff time.Duration
	// afterSettleTry is a test cutpoint after a failed settle try.
	afterSettleTry func()
	// afterWaitDone is a test barrier after a finished worker's settlement
	// attempt; nil in production.
	afterWaitDone func()
	// stopBackoff and stopSleep drive the bounded cleanup retries in tests.
	stopBackoff time.Duration
	stopSleep   func(context.Context, time.Duration) error
	// settleClock runs the settle backoff on fake time in tests.
	settleClock settle.Clock
	// afterFinishExec sees each finish command's result and may replace
	// its error, so a test can lose the reply of a committed finish
	// (goodqa#15087 AC-7 fault 2); nil in production.
	afterFinishExec func(resp store.Response, err error) error
	// dispatchQueries counts dispatch selections (AC-3: one per tick).
	dispatchQueries int

	// Counters for /metrics and the AC-2 assertion.
	Launched, WaitsResumed, JobsWoken, Expired, CapacityWaits int
}

func New(st *store.Store, cfg Config, ln Launcher) *Scheduler {
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if len(cfg.Holds) > 0 && cfg.HoldTTL <= 0 {
		cfg.HoldTTL = 2 * cfg.Wall
	}
	cfgErr := cfg.gateErr()
	if _, ok := ln.(Stopper); cfgErr == nil && cfg.ExitCleanup && !ok {
		cfgErr = fmt.Errorf("sched: ExitCleanup needs a launcher that can Stop and verify a worker")
	}
	return &Scheduler{st: st, cfg: cfg, ln: ln, cfgErr: cfgErr}
}

// gateErr refuses a gate that cannot keep its holds for a run (red-team F5):
// no wall-clock limit, a limit the hold cap cannot cover twice, a hold TTL
// out of range, or runners that would share the scheduler's database role
// (N3). Nothing is silently capped.
func (c Config) gateErr() error {
	// Every launch, gated or not, needs the separate runner role: a worker
	// must never run with the scheduler's FABRIC_DSN, nor with none at all.
	if strings.TrimSpace(c.RunnerDSN) == "" {
		return fmt.Errorf("sched: every launch needs a separate runner database role (RunnerDSN)")
	}
	if len(c.Holds) == 0 {
		return nil
	}
	switch {
	case c.Wall <= 0:
		return fmt.Errorf("sched: the dispatch gate needs a wall-clock limit (Wall > 0)")
	case 2*c.Wall > store.MaxHoldTTL:
		return fmt.Errorf("sched: wall %s is over half the hold cap %s", c.Wall, store.MaxHoldTTL)
	case c.HoldTTL < store.MinHoldTTL || c.HoldTTL > store.MaxHoldTTL:
		return fmt.Errorf("sched: hold ttl %s outside %s..%s", c.HoldTTL, store.MinHoldTTL, store.MaxHoldTTL)
	}
	return nil
}

// checkRunnerRole connects as the runner once and refuses a role that could
// act as the scheduler (red-team G2): otherwise 0015 would be vacuous.
func (s *Scheduler) checkRunnerRole(ctx context.Context) error {
	rs, err := store.Open(ctx, s.cfg.RunnerDSN)
	if err != nil {
		return fmt.Errorf("sched: runner database role: %w", err)
	}
	defer rs.Close()
	if err := rs.CheckRunnerRole(ctx); err != nil {
		return fmt.Errorf("sched: the dispatch gate refuses this runner role: %w", err)
	}
	return nil
}

// ensureRunnerRole refuses any launch until the configured runner login has
// passed CheckRunnerRole, gate or no gate (goodqa#15173): an owner or
// scheduler login handed to a worker would give it the scheduler's
// authority. Success is cached; a failure is retried next time, never skipped.
func (s *Scheduler) ensureRunnerRole(ctx context.Context) error {
	if s.cfgErr != nil {
		return s.cfgErr
	}
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	if s.runnerChecked {
		return nil
	}
	if err := s.checkRunnerRole(ctx); err != nil {
		return err
	}
	s.runnerChecked = true
	return nil
}

// runnerDSN is what a runner connects with: only the configured runner
// login, never the scheduler's own FABRIC_DSN (goodqa#15173, Kimi K1).
// gateErr guarantees it is non-empty before any launch.
func (s *Scheduler) runnerDSN() string {
	return s.cfg.RunnerDSN
}

// SetProber installs the runner readback the UNKNOWN reconciler uses (G1).
func (s *Scheduler) SetProber(p Prober) { s.prober = p }

func (s *Scheduler) gated() bool { return len(s.cfg.Holds) > 0 }

// Tick runs one pass of every sweep and reports whether it did any work.
func (s *Scheduler) Tick(ctx context.Context) (bool, error) {
	if err := s.ensureRunnerRole(ctx); err != nil {
		return false, err // a configuration or role fault is global: stop before any write
	}
	worked := false
	if n, err := s.consumeOutbox(ctx); err != nil {
		return false, err
	} else if n > 0 {
		worked = true
	}
	var exp []store.ExpiredLease
	if s.reconcileAllowed(ctx) {
		var err error
		exp, err = s.st.ExpireLeases(ctx, 50)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fabd: lease sweep:", err) // per-row failures never stop the loop (review H10)
		}
		s.Expired += len(exp)
		s.reconcileUnknown(ctx)
	}
	if s.gated() {
		if _, err := s.st.ExpireEndedReservations(ctx, 50); err != nil {
			fmt.Fprintln(os.Stderr, "fabd: reservation sweep:", err)
		}
	}
	if n, err := s.retireRequestedJobs(ctx); err != nil {
		return false, err
	} else if n > 0 {
		worked = true
	}
	if n, err := s.wakeExitedJobs(ctx); err != nil {
		return false, err
	} else if n > 0 {
		worked = true
	}
	if n, err := s.dispatch(ctx); err != nil {
		return false, err
	} else if n > 0 {
		worked = true
	}
	return worked || len(exp) > 0, nil
}

// Run loops until ctx ends: tick, then block on a wake hint or the backoff.
func (s *Scheduler) Run(ctx context.Context) error {
	conn, err := s.st.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+WakeChannel); err != nil {
		return err
	}
	backoff := s.cfg.Backoff
	for {
		worked, err := s.Tick(ctx)
		if err != nil {
			return err
		}
		if worked {
			backoff = s.cfg.Backoff
			continue // something changed; rescan before blocking (E6 pattern)
		}
		notified, err := waitForWake(ctx, func(wctx context.Context) error {
			_, err := conn.Conn().WaitForNotification(wctx)
			return err
		}, backoff, sleepContext)
		if err != nil {
			return err
		}
		if notified {
			backoff = s.cfg.Backoff
			continue
		}
		backoff = nextBackoff(backoff, s.cfg.MaxBackoff)
	}
}

// nextBackoff doubles cur up to max, never past it.
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur >= max {
		return max
	}
	return min(cur*2, max)
}

// waitForWake waits once for a LISTEN notification for up to delay. An
// expired wait already spent delay; an error while the wait context is
// still live (a broken LISTEN connection returns at once) gets its own
// bounded sleep, so Run never spins Tick in a hot loop.
func waitForWake(ctx context.Context, wait func(context.Context) error, delay time.Duration, sleep func(context.Context, time.Duration) error) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, delay)
	err := wait(wctx)
	expired := wctx.Err() != nil // read before cancel: cancel is not expiry
	cancel()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !expired {
		fmt.Fprintln(os.Stderr, "fabd: LISTEN wait failed, backing off:", err)
		if err := sleep(ctx, delay); err != nil {
			return false, err
		}
	}
	return false, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Notify pings the wake channel; writers call it after a commit.
func Notify(ctx context.Context, st *store.Store) {
	st.Pool().Exec(ctx, "NOTIFY "+WakeChannel) //nolint:errcheck
}

// consumeOutbox maintains the waits projection from the mission events. A
// mission whose stored event this build cannot decode is held (docs/
// envelope.md): the cursor still advances, the other missions keep flowing,
// and the held mission's rows are drained in order once they decode. Each row
// is applied in the same transaction as the cursor ack (store.ConsumeOutbox),
// so a crash can only redeliver. It returns the rows applied: skipped and
// newly held rows are not work, so a tick over only held rows is idle.
func (s *Scheduler) consumeOutbox(ctx context.Context) (int, error) {
	resumed := 0
	apply := func(ctx context.Context, tx pgx.Tx, r store.OutboxRow) error {
		switch r.Event.Kind {
		case domain.EvAttemptFinished:
			if r.Event.Outcome == "wait" {
				if _, err := tx.Exec(ctx, `INSERT INTO waits (mission_id, kind, key) VALUES ($1, $2, $3)
					ON CONFLICT (mission_id) DO UPDATE SET kind = EXCLUDED.kind, key = EXCLUDED.key, since = now()`,
					string(r.Mission), waitKind(r.Event.WaitKey), r.Event.WaitKey); err != nil {
					return err
				}
			}
		case domain.EvMissionResumed, domain.EvMissionReleased:
			if _, err := tx.Exec(ctx, "DELETE FROM waits WHERE mission_id = $1", string(r.Mission)); err != nil {
				return err
			}
			if r.Event.Kind == domain.EvMissionResumed {
				resumed++
				if _, err := tx.Exec(ctx, `INSERT INTO sched_counters (name, value) VALUES ('waits_resumed_total', 1) ON CONFLICT (name) DO UPDATE SET value = sched_counters.value + 1`); err != nil {
					return err
				}
			}
		}
		return nil
	}
	n, err := s.st.ConsumeOutbox(ctx, "sched", 200, apply)
	if err != nil {
		return 0, err
	}
	s.WaitsResumed += resumed
	resumed = 0
	d, err := s.st.DrainHolds(ctx, "sched", 200, apply)
	s.WaitsResumed += resumed
	if err != nil {
		return n, err
	}
	return n + d, nil
}

func waitKind(key string) string {
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i]
	}
	return "event"
}

func (s *Scheduler) bump(ctx context.Context, name string) {
	s.st.Pool().Exec(ctx, `INSERT INTO sched_counters (name, value) VALUES ($1, 1) ON CONFLICT (name) DO UPDATE SET value = sched_counters.value + 1`, name) //nolint:errcheck
}

// wakeExitedJobs turns each exited, un-woken job into a WAKE command with a
// deterministic key, so a repeated sweep replays instead of waking twice.
func (s *Scheduler) wakeExitedJobs(ctx context.Context) (int, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT id, mission_id FROM jobs WHERE state = 'EXITED' AND NOT woken
		AND NOT (mission_id = ANY($1)) AND NOT EXISTS (SELECT 1 FROM outbox_cursors hc WHERE hc.consumer = 'sched.hold.' || jobs.mission_id) ORDER BY ended_at LIMIT 50`, s.parkedIDs())
	if err != nil {
		return 0, err
	}
	type j struct{ id, mission string }
	var jobs []j
	for rows.Next() {
		var x j
		if err := rows.Scan(&x.id, &x.mission); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, x)
	}
	rows.Close()
	for _, x := range jobs {
		if s.prober != nil {
			// A runner records its own job's exit (0017), so the exit is a
			// claim: wake only once the job's unit is gone (main, after H1).
			if alive, known := s.prober.JobAlive(x.id); known && alive {
				s.park(domain.MissionID(x.mission), fmt.Errorf("job %s reported exited but its unit is still running", x.id))
				continue
			}
		}
		_, err := s.st.Execute(ctx, "fabd", domain.CommandID("wake:job:"+x.id), domain.Command{Kind: domain.CmdWake, Mission: domain.MissionID(x.mission), Token: "job:" + x.id})
		if err != nil && !errors.As(err, new(*store.Refusal)) {
			// Red-team D1: park this one mission and carry on. The job stays
			// un-woken, so it is retried once the park lapses.
			s.park(domain.MissionID(x.mission), err)
			continue
		}
		if _, err := s.st.Pool().Exec(ctx, "UPDATE jobs SET woken = true WHERE id = $1", x.id); err != nil {
			s.park(domain.MissionID(x.mission), err)
			continue
		}
		s.JobsWoken++
	}
	return len(jobs), nil
}

// dispatch launches one worker for each admitted mission with no live or
// unresolved attempt and no open wait. The launch is synchronous here: the
// worker runs to its end and its stream decides the finish. (fab-host will
// run these concurrently; one at a time keeps the shadow slice legible.)
func (s *Scheduler) dispatch(ctx context.Context) (int, error) {
	s.dispatchQueries++
	// goodqa#15087 AC-3: fabd launches claude-code only. The filter is in
	// SQL, inside the LIMIT window, so a run of anthropic-api missions at
	// higher priority can neither crowd a claude-code one out nor be
	// started, parked or retried here; they wait for `fab mission run`.
	rows, err := s.st.Pool().Query(ctx, `SELECT m.id FROM missions m
		WHERE m.execution = 'ADMITTED' AND m.terminal = 'NONE' AND m.provider = 'claude-code'
		  AND NOT EXISTS (SELECT 1 FROM waits w WHERE w.mission_id = m.id)
		  AND (m.not_before IS NULL OR m.not_before <= now())
		  AND NOT (m.id = ANY($1))
		  AND NOT EXISTS (SELECT 1 FROM outbox_cursors hc WHERE hc.consumer = 'sched.hold.' || m.id)
		  AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.mission_id = m.id
		                  AND a.state IN ('ADMITTED','LEASED','PREPARING','RUNNING','SUCCEEDED_UNVERIFIED','UNKNOWN','RECONCILING'))
		ORDER BY m.priority, m.id LIMIT 10`, s.parkedIDs())
	if err != nil {
		return 0, err
	}
	var ids []domain.MissionID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, domain.MissionID(id))
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if err := s.runOnce(ctx, id); err != nil {
			if errors.Is(err, errCapacityFull) {
				s.noteCapacityWait(ctx, id)
				continue // waiting, not failing: no attempt, no park, not work
			}
			// Red-team F1 and D1: a per-mission error never stops the loop.
			// The mission is parked in this process (an unreadable log until
			// restart, anything else for parkRetry); it is not work, so Run
			// backs off instead of spinning.
			s.park(id, err)
			continue
		}
		n++
	}
	return n, nil
}

// errCapacityFull: a configured resource has no room, so nothing started.
var errCapacityFull = errors.New("capacity full")

// capacityFull reports whether any configured hold cannot fit right now.
// A missing resource is not "full": the hold refuses it as a real fault.
func (s *Scheduler) capacityFull(ctx context.Context) (bool, error) {
	for _, h := range s.cfg.Holds {
		c, held, err := s.st.Capacity(ctx, h.Resource)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if h.Amount > c-held {
			return true, nil
		}
	}
	return false, nil
}

// parkRetry is how long a mission whose sweep step failed is left alone.
const parkRetry = time.Minute

func (s *Scheduler) park(id domain.MissionID, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parked == nil {
		s.parked = map[domain.MissionID]time.Time{}
	}
	until := time.Now().Add(parkRetry)
	if errors.Is(err, domain.ErrUnsupportedEvent) {
		until = time.Now().Add(100 * 365 * 24 * time.Hour) // a newer build owns it: until this process restarts
	}
	if _, was := s.parked[id]; !was {
		fmt.Fprintf(os.Stderr, "fabd: parked %s: %v\n", id, err)
	}
	s.parked[id] = until
}

func (s *Scheduler) parkedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	now := time.Now()
	for id, until := range s.parked {
		if now.After(until) {
			delete(s.parked, id) // the park lapsed: try the mission again
			continue
		}
		out = append(out, string(id))
	}
	return out
}

// runOnce = StartAttempt -> launch -> parse stream -> FinishAttempt -> Reconcile.
func (s *Scheduler) runOnce(ctx context.Context, mission domain.MissionID) error {
	// The launch boundary does not rely on Tick having checked the runner
	// role: runOnce is also reached directly by tests and embedders.
	if err := s.ensureRunnerRole(ctx); err != nil {
		return err
	}
	if s.beforeRun != nil {
		if err := s.beforeRun(mission); err != nil {
			return err
		}
	}
	if s.gated() {
		// Red-team N2: a resource known to be full starts nothing. The
		// mission waits without an attempt and is retried on the next tick
		// (a release pings the wake channel).
		if full, err := s.capacityFull(ctx); err != nil || full {
			if err != nil {
				return err
			}
			return errCapacityFull
		}
	}
	m, err := s.st.Mission(ctx, mission)
	if err != nil {
		return err
	}
	attempt := domain.AttemptID(fmt.Sprintf("r_%s_%d", strings.TrimPrefix(string(mission), "m_"), m.Version.AttemptFence+1))
	if s.afterRead != nil {
		s.afterRead()
	}
	// fabd launches the Claude Code binary only: its path is claude-code,
	// from its own identity, never read from the mission (goodqa#15087 F1).
	resp, err := s.st.Execute(ctx, "fabd", domain.CommandID("start:"+string(attempt)), domain.Command{Kind: domain.CmdStartAttempt, Mission: mission, Attempt: attempt, Host: s.cfg.Host, Role: "writer",
		Path: domain.ProviderClaudeCode})
	if err != nil {
		var ref *store.Refusal
		if errors.As(err, &ref) {
			return nil // someone else moved the mission; the next tick sees the new state
		}
		return err
	}
	// S1: a replayed START means another scheduler (or an earlier call of
	// this one) executed it and owns the launch. Only the executor launches.
	if resp.Replayed {
		return nil
	}
	fence := resp.Events[0].Fence
	if s.gated() {
		// C5: hold before any preparation; a refusal launches nothing.
		if _, err := s.st.Hold(ctx, store.HoldRequest{Key: "hold:" + string(attempt), Attempt: attempt, Fence: fence,
			Items: s.cfg.Holds, TTL: s.cfg.HoldTTL}); err != nil {
			return s.abandonPrelaunchWait(ctx, mission, attempt, fence, "hold refused: "+err.Error(), errors.Is(err, domain.ErrOverCapacity))
		}
	}

	worktree := filepath.Join(s.cfg.WorkRoot, "wt", string(mission))
	runDir := filepath.Join(s.cfg.WorkRoot, "run", string(attempt))
	for _, d := range []string{worktree, runDir, filepath.Join(runDir, "jobs"), filepath.Join(runDir, "claude-config"), filepath.Join(runDir, "cache"), filepath.Join(runDir, "config")} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			// Red-team N1: nothing launched yet; abandon so the holds go now.
			return s.abandonPrelaunch(ctx, mission, attempt, fence, "run directory not prepared: "+err.Error())
		}
		os.Chmod(d, 0o775|os.ModeSetgid) //nolint:errcheck // group-shared with the worker account
	}
	// The checkpoint and the event window come from one verified snapshot read
	// after START (Kimi projection analysis): the mission read before START
	// would say fence 0 with no live attempt while the window shows its start.
	// Nothing is launched yet, so an unverifiable snapshot abandons cleanly.
	cur, events, err := s.st.VerifiedSnapshot(ctx, mission)
	if err != nil {
		aerr := s.abandonPrelaunch(ctx, mission, attempt, fence, "snapshot not verified: "+err.Error())
		// F1a: a log this build cannot decode holds the mission, at the
		// current scheduler cursor so rows already applied are never
		// redelivered. The abandon comes first, so a crash between the two
		// cannot leave a live attempt on a held mission (the next tick
		// starts, fails and abandons again).
		if envelope.Undecodable(err) {
			if herr := s.st.HoldMission(ctx, s.st.Pool(), mission); herr != nil {
				fmt.Fprintln(os.Stderr, "fabd: hold:", herr)
			} else {
				fmt.Fprintf(os.Stderr, "fabd: held %s: its log cannot be decoded by this build\n", mission)
			}
		}
		return aerr
	}
	proj, err := projection.Build(cur, events, projection.DefaultCapTokens, "continue the mission; end with the checkpoint schema")
	if err != nil {
		return s.abandonPrelaunch(ctx, mission, attempt, fence, "projection budget fault: "+err.Error())
	}
	prompt, _ := json.Marshal(proj)
	g := host.Grant{Run: attempt, Mission: mission, Fence: fence, Model: s.cfg.Model, Effort: s.cfg.Effort, Rung: s.cfg.Rung,
		TierTableSHA: s.cfg.TierTableSHA, ConfigSHA: "n/a", Profile: s.cfg.Profile, MaxTurns: s.cfg.MaxTurns, MaxBudgetUSD: s.cfg.MaxBudgetUSD,
		Wall: s.cfg.Wall, Worktree: worktree, RunDir: runDir, Token: fmt.Sprintf("%s-%d-%s", attempt, fence, randomToken())}
	l := s.cfg.Launch
	l.Prompt = string(prompt)
	argv, env, err := claude.Argv(g, l)
	if err != nil {
		return s.abandonPrelaunch(ctx, mission, attempt, fence, "launch refused: "+err.Error())
	}
	env = append(env, "FAB_RUN_DIR="+runDir, "FAB_WORKTREE="+worktree, "FABRIC_HOME="+filepath.Join(runDir, "fabric-home"))
	if dsn := s.runnerDSN(); dsn != "" {
		env = append(env, "FABRIC_DSN="+dsn)
	}
	// S2: ACK_START commits before any runner bytes, so the pre-launch
	// ownership boundary is durable first. A failed ACK launches nothing.
	if _, err := s.st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(attempt)), domain.Command{Kind: domain.CmdAckStart, Mission: mission, Attempt: attempt, Fence: fence}); err != nil {
		return err
	}
	if s.afterAck != nil {
		s.afterAck()
	}
	if !s.gated() {
		// goodqa#15082 T7: final admission is mandatory in every deployment.
		// Ungated, the same lease and intent checks run without a grant row.
		reason, err := s.st.CheckDispatchFresh(ctx, attempt, fence)
		switch {
		case err != nil:
			return s.abandonPrelaunch(ctx, mission, attempt, fence, "dispatch not authorized: "+err.Error())
		case reason != "":
			return s.abandonPrelaunch(ctx, mission, attempt, fence, "dispatch denied: "+reason)
		}
	}
	if s.gated() {
		// C5: the final grant, after ACK and before any runner byte. A
		// denial is definite (nothing started), so the attempt is abandoned.
		need := make([]string, len(s.cfg.Holds))
		for i, h := range s.cfg.Holds {
			need[i] = h.Resource
		}
		gr, err := s.st.AuthorizeDispatch(ctx, attempt, fence, need)
		switch {
		case err != nil:
			return s.abandonPrelaunch(ctx, mission, attempt, fence, "dispatch not authorized: "+err.Error())
		case gr.Replayed:
			return nil // decided by an earlier call, which owns the launch
		case !gr.Granted:
			return s.abandonPrelaunch(ctx, mission, attempt, fence, "dispatch denied: "+gr.Reason)
		}
	}
	out, err := os.Create(filepath.Join(runDir, "stream.jsonl"))
	if err != nil {
		return s.abandonPrelaunch(ctx, mission, attempt, fence, "stream file not created: "+err.Error())
	}
	errf, _ := os.Create(filepath.Join(runDir, "stderr.log"))
	h, err := s.ln.Start(g, argv, env, out, errf)
	if err != nil {
		// S4: a start error is ambiguous (the unit may exist). Never a
		// definite abandonment: the acked attempt keeps its lease, which
		// expires into UNKNOWN for reconciliation, so the next pass cannot
		// launch a second worker beside a possibly live one.
		out.Close()
		if errf != nil {
			errf.Close()
		}
		fmt.Fprintf(os.Stderr, "fabd: start of %s is ambiguous, left to lease expiry: %v\n", attempt, err)
		return nil
	}
	// S3: count physical starts, not selected candidates.
	s.Launched++
	s.bump(ctx, "runs_launched_total")
	// The worker runs while the loop keeps serving its job requests and
	// wakes; its end is settled from the stream when it exits.
	done := make(chan struct{})
	go s.heartbeat(ctx, attempt, fence, done)
	go func() {
		code, werr := s.ln.Wait(h)
		out.Close()
		if errf != nil {
			errf.Close()
		}
		if s.cfg.ExitCleanup {
			// Leader exit does not reap descendants. A Wait error does not end
			// ownership either, so cleanup runs either way, detached from the
			// scheduler context; a failure makes completion unverified.
			werr = errors.Join(werr, s.cleanupHandle(context.Background(), h, "leader exited; verify descendant cleanup"))
		}
		if werr != nil {
			close(done) // the exit is unknown: stop beating so the lease expires
		}
		// T6: with an observed exit the heartbeat keeps the lease while the
		// settle (and its retries) persist it.
		if err := s.afterWait(context.Background(), mission, attempt, fence, runDir, code, werr); err != nil {
			fmt.Fprintln(os.Stderr, "fabd: settle:", err)
		}
		if s.afterWaitDone != nil {
			s.afterWaitDone()
		}
		if werr == nil {
			close(done)
		}
		Notify(context.Background(), s.st)
	}()
	return nil
}

// cleanupHandle requires a verified Stop receipt with no survivors and no
// escaped processes, retrying a bounded three times with a doubling pause.
func (s *Scheduler) cleanupHandle(ctx context.Context, h host.Handle, reason string) error {
	stopper, ok := s.ln.(Stopper)
	if !ok {
		return fmt.Errorf("descendant cleanup for %s: launcher cannot Stop", h.Run)
	}
	const tries = 3
	wait := s.stopBackoff
	if wait <= 0 {
		wait = 10 * time.Millisecond
	}
	sleep := s.stopSleep
	if sleep == nil {
		sleep = sleepContext
	}
	var lastErr error
	for try := 1; try <= tries; try++ {
		rc, err := stopper.Stop(h, reason, 2*time.Second)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("descendant cleanup for %s failed after %d attempt(s): %w", h.Run, try, err)
		case !rc.Verified:
			lastErr = fmt.Errorf("descendant cleanup for %s was not verified after %d attempt(s)", h.Run, try)
		case len(rc.Survivors) > 0:
			lastErr = fmt.Errorf("descendant cleanup for %s left %d survivors after %d attempt(s)", h.Run, len(rc.Survivors), try)
		case len(rc.Escaped) > 0:
			lastErr = fmt.Errorf("descendant cleanup for %s found %d escaped processes after %d attempt(s)", h.Run, len(rc.Escaped), try)
		default:
			return nil
		}
		if try < tries {
			fmt.Fprintf(os.Stderr, "fabd: cleanup %s try %d failed, retrying: %v\n", h.Run, try, lastErr)
			if err := sleep(ctx, wait); err != nil {
				return fmt.Errorf("descendant cleanup for %s retry interrupted: %w", h.Run, err)
			}
			wait *= 2
		}
	}
	return lastErr
}

// afterWait settles a finished worker. A failed Wait (or, with ExitCleanup,
// a failed descendant cleanup) means completion is not known (goodqa#15082
// T1): nothing is settled, the heartbeat has stopped, and the lease expires
// into UNKNOWN for the prober to reconcile.
func (s *Scheduler) afterWait(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, runDir string, code int, werr error) error {
	if werr != nil {
		return fmt.Errorf("wait for %s failed, left to lease expiry: %w", attempt, werr)
	}
	// goodqa#15082 T6, goodqa#15087 AC-7: the exit is observed; a transient
	// store error must not drop it. The finish command is decided once from
	// the stream (T1: an unreadable stream settles nothing and the lease
	// expires into UNKNOWN), and its store calls are idempotent (finish: and
	// verify: command ids), so settle.Finish retries them while the caller
	// keeps the heartbeat going.
	cmd, err := settleCommand(mission, attempt, fence, runDir, code)
	if err != nil || cmd == nil {
		return err
	}
	return s.finishRetry(ctx, *cmd)
}

// finishRetry is every FINISH_ATTEMPT fabd sends: one finishCmd under
// settle.Finish, so a transient store error never drops an observed end
// or a prelaunch abandon (goodqa#15087 AC-7).
func (s *Scheduler) finishRetry(ctx context.Context, cmd domain.Command) error {
	return settle.Finish(ctx, settle.Options{Clock: s.settleClock, Backoff: s.settleBackoff, Max: s.st.Lease / 3,
		Log: func(try int, err error) {
			fmt.Fprintf(os.Stderr, "fabd: settle %s try %d failed, retrying: %v\n", cmd.Attempt, try, err)
			if s.afterSettleTry != nil {
				s.afterSettleTry()
			}
		}}, func(ctx context.Context) error { return s.finishCmd(ctx, cmd) })
}

// heartbeat keeps a launched attempt's lease alive while its runner runs
// (red-team G1): without it every run longer than the lease went UNKNOWN.
// It stops when the runner ends, when this scheduler's context ends (a
// crash or shutdown: the lease then lapses into UNKNOWN for the reconciler)
// or when the attempt is no longer live.
func (s *Scheduler) heartbeat(ctx context.Context, attempt domain.AttemptID, fence int64, done <-chan struct{}) {
	every := s.st.Lease / 3
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			err := error(nil)
			if s.beforeHeartbeat != nil {
				err = s.beforeHeartbeat()
			}
			if err == nil {
				err = s.st.Heartbeat(ctx, attempt, fence)
			}
			switch {
			case err == nil:
			case errors.Is(err, domain.ErrStaleFence), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrNotFound):
				// Definite: another fence owns the attempt or it is no longer live.
				fmt.Fprintf(os.Stderr, "fabd: heartbeat %s stopped: %v\n", attempt, err)
				return
			default:
				// Red-team H2: transient (a dropped connection, a timeout). Keep
				// beating; the lease is three beats long.
				if ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "fabd: heartbeat %s failed, retrying: %v\n", attempt, err)
				}
			}
		}
	}
}

// reconcileUnknown reads back each UNKNOWN attempt of this host (red-team
// G1). A runner that is provably gone is quarantined: it ran, or may have,
// and its outcome is unverified, so the mission pauses for review instead
// of being retried blind, and its holds are released by the sweep. A live
// runner, or no answer, leaves the attempt UNKNOWN and its capacity held.
//
// An anthropic-api attempt is read back through the flock on the lock path
// stored in its row, never a recomputed one (goodqa#15087); every other
// attempt through the Prober. It returns how many it quarantined.
func (s *Scheduler) reconcileUnknown(ctx context.Context) int {
	if s.prober == nil && s.lockProber == nil {
		return 0
	}
	rows, err := s.st.Pool().Query(ctx, `SELECT id, mission_id, fence, COALESCE(path, ''), COALESCE(lock_path, '') FROM attempts
		WHERE state = 'UNKNOWN' AND host = $1
		AND NOT EXISTS (SELECT 1 FROM outbox_cursors hc WHERE hc.consumer = 'sched.hold.' || attempts.mission_id) ORDER BY started_at LIMIT 20`, string(s.cfg.Host))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabd: unknown sweep:", err)
		return 0
	}
	type u struct {
		id             domain.AttemptID
		mission        domain.MissionID
		fence          int64
		path, lockPath string
	}
	var page []u
	for rows.Next() {
		var x u
		if err := rows.Scan(&x.id, &x.mission, &x.fence, &x.path, &x.lockPath); err != nil {
			rows.Close()
			return 0
		}
		page = append(page, x)
	}
	rows.Close()
	quarantined := 0
	for _, x := range page {
		var alive, known bool
		switch {
		case domain.Provider(x.path) == domain.ProviderAnthropicAPI && s.lockProber != nil:
			alive, known = s.lockProber(x.id, x.lockPath), true
		case domain.Provider(x.path) != domain.ProviderAnthropicAPI && s.prober != nil:
			alive, known = s.prober.Alive(x.id)
		}
		if !known || alive {
			continue
		}
		_, err := s.st.Execute(ctx, "fabd", domain.CommandID("reconcile:gone:"+string(x.id)), domain.Command{Kind: domain.CmdReconcileAttempt,
			Mission: x.mission, Attempt: x.id, Fence: x.fence, Result: "QUARANTINED", Reason: "runner gone after its lease lapsed; outcome unverified"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fabd: reconcile %s: %v\n", x.id, err) // per-attempt: never stops the loop
			continue
		}
		fmt.Fprintf(os.Stderr, "fabd: quarantined %s: runner gone, outcome unverified\n", x.id)
		s.bump(ctx, "attempts_quarantined_total")
		quarantined++
	}
	return quarantined
}

// noteCapacityWait records that a mission is waiting for capacity and,
// once per mission, names what holds it (red-team G1: a wait leaves no
// event, so it must leave a signal).
func (s *Scheduler) noteCapacityWait(ctx context.Context, mission domain.MissionID) {
	s.CapacityWaits++
	s.bump(ctx, "capacity_waits_total")
	s.mu.Lock()
	if s.waitLogged == nil {
		s.waitLogged = map[domain.MissionID]bool{}
	}
	first := !s.waitLogged[mission]
	s.waitLogged[mission] = true
	s.mu.Unlock()
	if !first {
		return
	}
	for _, h := range s.cfg.Holds {
		rows, err := s.st.Pool().Query(ctx, `SELECT r.attempt_id, COALESCE(a.state, '?'), r.created_at FROM reservations r
			LEFT JOIN attempts a ON a.id = r.attempt_id WHERE r.resource = $1 AND r.state = 'HELD' ORDER BY r.created_at LIMIT 5`, h.Resource)
		if err != nil {
			continue
		}
		for rows.Next() {
			var a, st string
			var since time.Time
			if rows.Scan(&a, &st, &since) == nil {
				fmt.Fprintf(os.Stderr, "fabd: %s waiting for capacity on %s: held by %s (%s) since %s\n", mission, h.Resource, a, st, since.UTC().Format(time.RFC3339))
			}
		}
		rows.Close()
	}
}

// settle reads the finished worker's stream and ends the attempt.
func (s *Scheduler) settle(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, runDir string, code int) error {
	cmd, err := settleCommand(mission, attempt, fence, runDir, code)
	if err != nil || cmd == nil {
		return err
	}
	return s.finishRetry(ctx, *cmd)
}

// settleCommand reads the stream once and decides the finish command; nil
// means no result event (lease expiry to UNKNOWN). Retries reuse the command
// it returns, so a stream that changes between tries can never produce a
// second, conflicting finish under the same command id (T6 review N1).
func settleCommand(mission domain.MissionID, attempt domain.AttemptID, fence int64, runDir string, code int) (*domain.Command, error) {
	fin := func(outcome, waitKey, reason string) (*domain.Command, error) {
		return &domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: attempt, Fence: fence, Outcome: outcome, WaitKey: waitKey, Reason: reason}, nil
	}
	f, err := os.Open(filepath.Join(runDir, "stream.jsonl"))
	if err != nil {
		return nil, err
	}
	run, perr := claude.Parse(f, true)
	f.Close()
	if perr != nil {
		return fin("abandoned", "", "stream unreadable: "+perr.Error())
	}
	usage, _ := json.Marshal(map[string]any{"session": run.SessionID, "turns": run.Turns, "context_last_turn": run.LastTurn.Context(), "cost_usd": run.TotalCostUSD, "exit": code})
	os.WriteFile(filepath.Join(runDir, "usage.json"), usage, 0o644) //nolint:errcheck
	switch run.End {
	case claude.EndCheckpoint:
		// goodqa#15082 T1: success needs the process to agree (exit 0) and
		// the stream to be coherent; otherwise the run is abandoned, never
		// verified on the strength of its last event.
		if code != 0 {
			return fin("abandoned", "", fmt.Sprintf("exit %d with a success result", code))
		}
		if len(run.Contradictions) > 0 {
			return fin("abandoned", "", "contradictory stream: "+run.Contradictions[0])
		}
		var cp struct {
			End struct {
				Kind  string `json:"kind"`
				Event string `json:"event"`
			} `json:"end"`
		}
		if err := json.Unmarshal(run.StructuredOutput, &cp); err != nil || cp.End.Kind == "" {
			return fin("abandoned", "", "checkpoint without an end kind")
		}
		switch cp.End.Kind {
		case "wait":
			return fin("wait", cp.End.Event, "")
		case "checkpoint", "ready_for_review", "blocked":
			return fin(cp.End.Kind, "", "")
		default:
			return fin("abandoned", "", "unknown end kind "+cp.End.Kind)
		}
	case claude.EndAbandoned:
		return fin("abandoned", "", "no structured output")
	case claude.EndFailed:
		return fin("abandoned", "", "result "+run.ResultSubtype)
	default:
		// No result event: the host and provider do not agree; the attempt
		// goes UNKNOWN through lease expiry, never to a relaunch (3.4).
		return nil, nil
	}
}

// finish ends the attempt and, when the stream and exit agree, verifies it.
// abandonPrelaunch ends an attempt whose runner never started. The reducer
// backs the mission off and, after a run of these, parks it (red-team F1).
func (s *Scheduler) abandonPrelaunch(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, reason string) error {
	return s.abandonPrelaunchWait(ctx, mission, attempt, fence, reason, false)
}

// abandonPrelaunchWait with wait set marks the cause as a full resource:
// the mission backs off but the attempt does not count toward parking (D2).
func (s *Scheduler) abandonPrelaunchWait(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, reason string, wait bool) error {
	return s.finishRetry(ctx, domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: attempt, Fence: fence,
		Outcome: "abandoned", Prelaunch: true, CapacityWait: wait, Reason: reason})
}

func (s *Scheduler) finish(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, outcome, waitKey, reason string) error {
	return s.finishRetry(ctx, domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: attempt, Fence: fence, Outcome: outcome, WaitKey: waitKey, Reason: reason})
}

func (s *Scheduler) finishCmd(ctx context.Context, cmd domain.Command) error {
	mission, attempt, fence, outcome := cmd.Mission, cmd.Attempt, cmd.Fence, cmd.Outcome
	if s.gated() {
		defer s.releaseHolds(ctx, attempt)
	}
	resp, err := s.st.Execute(ctx, "fabd", domain.CommandID("finish:"+string(attempt)), cmd)
	if s.afterFinishExec != nil {
		err = s.afterFinishExec(resp, err)
	}
	if err != nil {
		if errors.As(err, new(*store.Refusal)) {
			return nil
		}
		return err
	}
	if outcome == "abandoned" {
		return nil // FAILED_DEFINITE already; AC-3 relaunch policy is the daemon's next step
	}
	_, err = s.st.Execute(ctx, "fabd", domain.CommandID("verify:"+string(attempt)), domain.Command{Kind: domain.CmdReconcileAttempt, Mission: mission, Attempt: attempt, Fence: fence, Result: "VERIFIED"})
	if err != nil && !errors.As(err, new(*store.Refusal)) {
		return err
	}
	return nil
}

// releaseHolds closes the attempt's held reservations; the expiry sweep is
// the backstop when this fails.
func (s *Scheduler) releaseHolds(ctx context.Context, attempt domain.AttemptID) {
	rs, err := s.st.Reservations(ctx, attempt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabd: release holds:", err)
		return
	}
	released := false
	for _, r := range rs {
		if r.State == store.ResHeld {
			if res, err := s.st.Release(ctx, r.ID); err != nil {
				fmt.Fprintln(os.Stderr, "fabd: release", r.ID+":", err)
			} else if res.Changed {
				released = true
			}
		}
	}
	if released {
		Notify(ctx, s.st) // N2: missions waiting for this capacity can try now
	}
}

func randomToken() string {
	b := make([]byte, 8)
	f, err := os.Open("/dev/urandom")
	if err == nil {
		f.Read(b) //nolint:errcheck
		f.Close()
	}
	return fmt.Sprintf("%x", b)
}

// Job records and watches one host job. JobRow is its state.
type JobRow struct {
	ID       string
	Mission  domain.MissionID
	Attempt  domain.AttemptID
	Fence    int64
	State    string
	ExitCode *int
	LogTail  string
	Woken    bool
}

// RequestJob is refused: host jobs are disabled (goodqa#15082 F0). The
// argv came from the confined run and the daemon launched it outside the
// sandbox. Nothing is read or written.
func RequestJob(ctx context.Context, st *store.Store, id string, mission domain.MissionID, attempt domain.AttemptID, fence int64, argv []string, rundir string) error {
	return ErrHostJobsDisabled
}

// retireRequestedJobs never launches a job. It moves each REQUESTED job of
// an attempt on this host to FAILED_TO_START; rows of other hosts, or with
// no matching attempt, are left alone. The host predicate is repeated in
// the update, and one row's failure does not stop the others.
func (s *Scheduler) retireRequestedJobs(ctx context.Context) (int, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT j.id FROM jobs j JOIN attempts a ON a.id = j.attempt_id AND a.mission_id = j.mission_id
		WHERE j.state = 'REQUESTED' AND a.host = $1 ORDER BY j.id LIMIT 20`, string(s.cfg.Host))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		tag, err := s.st.Pool().Exec(ctx, `UPDATE jobs j SET state = 'FAILED_TO_START', log_tail = 'host jobs are disabled (goodqa#15082)'
			FROM attempts a WHERE j.id = $1 AND j.state = 'REQUESTED' AND a.id = j.attempt_id AND a.mission_id = j.mission_id AND a.host = $2`, id, string(s.cfg.Host))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fabd: retire job", id+":", err)
			continue
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// FinishJob records the exit and pings the wake channel.
func FinishJob(ctx context.Context, st *store.Store, id string, exitCode int, logTail string) error {
	if len(logTail) > 4096 {
		logTail = logTail[len(logTail)-4096:]
	}
	_, err := st.Pool().Exec(ctx, `UPDATE jobs SET state = 'EXITED', exit_code = $2, log_tail = $3, ended_at = now() WHERE id = $1 AND state = 'STARTED'`, id, exitCode, logTail)
	if err != nil {
		return err
	}
	Notify(ctx, st)
	return nil
}

// LoadJob reads one job.
func LoadJob(ctx context.Context, st *store.Store, id string) (JobRow, error) {
	var j JobRow
	err := st.Pool().QueryRow(ctx, "SELECT id, mission_id, attempt_id, fence, state, exit_code, log_tail, woken FROM jobs WHERE id = $1", id).Scan(
		&j.ID, &j.Mission, &j.Attempt, &j.Fence, &j.State, &j.ExitCode, &j.LogTail, &j.Woken)
	return j, err
}
