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
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/projection"
	"fabric/internal/provider/claude"
	"fabric/internal/store"
)

// WakeChannel is the NOTIFY channel every writer pings after a commit that
// may unblock the scheduler.
const WakeChannel = "fabric_wake"

// Launcher is how the scheduler starts and waits for one worker process.
type Launcher interface {
	Start(g host.Grant, argv []string, env []string, stdout, stderr *os.File) (host.Handle, error)
	Wait(h host.Handle) (int, error)
	// StartJob runs a host job in its own unit or detached process under the
	// worker identity; it returns once the job is proven started.
	StartJob(unit string, argv []string, env []string) error
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
	HoldTTL time.Duration // default 2 x Wall, at most store.MaxHoldTTL
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
	// Counters for /metrics and the AC-2 assertion.
	Launched, WaitsResumed, JobsWoken, Expired int
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
	if cfg.HoldTTL > store.MaxHoldTTL {
		cfg.HoldTTL = store.MaxHoldTTL
	}
	return &Scheduler{st: st, cfg: cfg, ln: ln}
}

func (s *Scheduler) gated() bool { return len(s.cfg.Holds) > 0 }

// Tick runs one pass of every sweep and reports whether it did any work.
func (s *Scheduler) Tick(ctx context.Context) (bool, error) {
	worked := false
	if n, err := s.consumeOutbox(ctx); err != nil {
		return false, err
	} else if n > 0 {
		worked = true
	}
	exp, err := s.st.ExpireLeases(ctx, 50)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabd: lease sweep:", err) // per-row failures never stop the loop (review H10)
	}
	s.Expired += len(exp)
	if s.gated() {
		if _, err := s.st.ExpireReservations(ctx, 50); err != nil {
			fmt.Fprintln(os.Stderr, "fabd: reservation sweep:", err)
		}
	}
	if n, err := s.startRequestedJobs(ctx); err != nil {
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
		wctx, cancel := context.WithTimeout(ctx, backoff)
		_, nerr := conn.Conn().WaitForNotification(wctx)
		cancel()
		if nerr == nil {
			backoff = s.cfg.Backoff
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if backoff < s.cfg.MaxBackoff {
			backoff *= 2
		}
	}
}

// Notify pings the wake channel; writers call it after a commit.
func Notify(ctx context.Context, st *store.Store) {
	st.Pool().Exec(ctx, "NOTIFY "+WakeChannel) //nolint:errcheck
}

// consumeOutbox maintains the waits projection from the mission events.
func (s *Scheduler) consumeOutbox(ctx context.Context) (int, error) {
	rows, err := s.st.OutboxPage(ctx, "sched", 200)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	for _, r := range rows {
		switch r.Event.Kind {
		case domain.EvAttemptFinished:
			if r.Event.Outcome == "wait" {
				if _, err := s.st.Pool().Exec(ctx, `INSERT INTO waits (mission_id, kind, key) VALUES ($1, $2, $3)
					ON CONFLICT (mission_id) DO UPDATE SET kind = EXCLUDED.kind, key = EXCLUDED.key, since = now()`,
					string(r.Mission), waitKind(r.Event.WaitKey), r.Event.WaitKey); err != nil {
					return 0, err
				}
			}
		case domain.EvMissionResumed, domain.EvMissionReleased:
			if _, err := s.st.Pool().Exec(ctx, "DELETE FROM waits WHERE mission_id = $1", string(r.Mission)); err != nil {
				return 0, err
			}
			if r.Event.Kind == domain.EvMissionResumed {
				s.WaitsResumed++
				s.bump(ctx, "waits_resumed_total")
			}
		}
	}
	return len(rows), s.st.AckOutbox(ctx, "sched", rows[len(rows)-1].ID)
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
	rows, err := s.st.Pool().Query(ctx, `SELECT id, mission_id FROM jobs WHERE state = 'EXITED' AND NOT woken ORDER BY ended_at LIMIT 50`)
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
		_, err := s.st.Execute(ctx, "fabd", domain.CommandID("wake:job:"+x.id), domain.Command{Kind: domain.CmdWake, Mission: domain.MissionID(x.mission), Token: "job:" + x.id})
		if err != nil && !errors.As(err, new(*store.Refusal)) {
			return 0, err
		}
		if _, err := s.st.Pool().Exec(ctx, "UPDATE jobs SET woken = true WHERE id = $1", x.id); err != nil {
			return 0, err
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
	rows, err := s.st.Pool().Query(ctx, `SELECT m.id FROM missions m
		WHERE m.execution = 'ADMITTED' AND m.terminal = 'NONE'
		  AND NOT EXISTS (SELECT 1 FROM waits w WHERE w.mission_id = m.id)
		  AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.mission_id = m.id
		                  AND a.state IN ('ADMITTED','LEASED','PREPARING','RUNNING','SUCCEEDED_UNVERIFIED','UNKNOWN','RECONCILING'))
		ORDER BY m.priority, m.id LIMIT 10`)
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
	for _, id := range ids {
		if err := s.runOnce(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// runOnce = StartAttempt -> launch -> parse stream -> FinishAttempt -> Reconcile.
func (s *Scheduler) runOnce(ctx context.Context, mission domain.MissionID) error {
	m, err := s.st.Mission(ctx, mission)
	if err != nil {
		return err
	}
	attempt := domain.AttemptID(fmt.Sprintf("r_%s_%d", strings.TrimPrefix(string(mission), "m_"), m.Version.AttemptFence+1))
	if s.afterRead != nil {
		s.afterRead()
	}
	resp, err := s.st.Execute(ctx, "fabd", domain.CommandID("start:"+string(attempt)), domain.Command{Kind: domain.CmdStartAttempt, Mission: mission, Attempt: attempt, Host: s.cfg.Host, Role: "writer"})
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
			return s.finish(ctx, mission, attempt, fence, "abandoned", "", "hold refused: "+err.Error())
		}
	}

	worktree := filepath.Join(s.cfg.WorkRoot, "wt", string(mission))
	runDir := filepath.Join(s.cfg.WorkRoot, "run", string(attempt))
	for _, d := range []string{worktree, runDir, filepath.Join(runDir, "jobs"), filepath.Join(runDir, "claude-config"), filepath.Join(runDir, "cache"), filepath.Join(runDir, "config")} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			return err
		}
		os.Chmod(d, 0o775|os.ModeSetgid) //nolint:errcheck // group-shared with the worker account
	}
	// The checkpoint and the event window come from one verified snapshot read
	// after START (Kimi projection analysis): the mission read before START
	// would say fence 0 with no live attempt while the window shows its start.
	// Nothing is launched yet, so an unverifiable snapshot abandons cleanly.
	cur, events, err := s.st.VerifiedSnapshot(ctx, mission)
	if err != nil {
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "snapshot not verified: "+err.Error())
	}
	proj, err := projection.Build(cur, events, projection.DefaultCapTokens, "continue the mission; end with the checkpoint schema")
	if err != nil {
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "projection budget fault: "+err.Error())
	}
	prompt, _ := json.Marshal(proj)
	g := host.Grant{Run: attempt, Mission: mission, Fence: fence, Model: s.cfg.Model, Effort: s.cfg.Effort, Rung: s.cfg.Rung,
		TierTableSHA: s.cfg.TierTableSHA, ConfigSHA: "n/a", Profile: s.cfg.Profile, MaxTurns: s.cfg.MaxTurns, MaxBudgetUSD: s.cfg.MaxBudgetUSD,
		Wall: s.cfg.Wall, Worktree: worktree, RunDir: runDir, Token: fmt.Sprintf("%s-%d-%s", attempt, fence, randomToken())}
	l := s.cfg.Launch
	l.Prompt = string(prompt)
	argv, env, err := claude.Argv(g, l)
	if err != nil {
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "launch refused: "+err.Error())
	}
	env = append(env, "FAB_RUN_DIR="+runDir, "FAB_WORKTREE="+worktree, "FABRIC_DSN="+os.Getenv("FABRIC_DSN"), "FABRIC_HOME="+filepath.Join(runDir, "fabric-home"))
	// S2: ACK_START commits before any runner bytes, so the pre-launch
	// ownership boundary is durable first. A failed ACK launches nothing.
	if _, err := s.st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(attempt)), domain.Command{Kind: domain.CmdAckStart, Mission: mission, Attempt: attempt, Fence: fence}); err != nil {
		return err
	}
	if s.gated() {
		// C5: the final grant, after ACK and before any runner byte. A
		// denial is definite (nothing started), so the attempt is abandoned.
		need := make([]string, len(s.cfg.Holds))
		for i, h := range s.cfg.Holds {
			need[i] = h.Resource
		}
		if s.afterAck != nil {
			s.afterAck()
		}
		gr, err := s.st.AuthorizeDispatch(ctx, attempt, fence, need)
		switch {
		case err != nil:
			return s.finish(ctx, mission, attempt, fence, "abandoned", "", "dispatch not authorized: "+err.Error())
		case gr.Replayed:
			return nil // decided by an earlier call, which owns the launch
		case !gr.Granted:
			return s.finish(ctx, mission, attempt, fence, "abandoned", "", "dispatch denied: "+gr.Reason)
		}
	}
	out, err := os.Create(filepath.Join(runDir, "stream.jsonl"))
	if err != nil {
		return err
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
	go func() {
		code, _ := s.ln.Wait(h)
		out.Close()
		if errf != nil {
			errf.Close()
		}
		if err := s.settle(context.Background(), mission, attempt, fence, runDir, code); err != nil {
			fmt.Fprintln(os.Stderr, "fabd: settle:", err)
		}
		Notify(context.Background(), s.st)
	}()
	return nil
}

// settle reads the finished worker's stream and ends the attempt.
func (s *Scheduler) settle(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, runDir string, code int) error {
	f, err := os.Open(filepath.Join(runDir, "stream.jsonl"))
	if err != nil {
		return err
	}
	run, perr := claude.Parse(f, true)
	f.Close()
	if perr != nil {
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "stream unreadable: "+perr.Error())
	}
	usage, _ := json.Marshal(map[string]any{"session": run.SessionID, "turns": run.Turns, "context_last_turn": run.LastTurn.Context(), "cost_usd": run.TotalCostUSD, "exit": code})
	os.WriteFile(filepath.Join(runDir, "usage.json"), usage, 0o644) //nolint:errcheck
	switch run.End {
	case claude.EndCheckpoint:
		var cp struct {
			End struct {
				Kind  string `json:"kind"`
				Event string `json:"event"`
			} `json:"end"`
		}
		if err := json.Unmarshal(run.StructuredOutput, &cp); err != nil || cp.End.Kind == "" {
			return s.finish(ctx, mission, attempt, fence, "abandoned", "", "checkpoint without an end kind")
		}
		switch cp.End.Kind {
		case "wait":
			return s.finish(ctx, mission, attempt, fence, "wait", cp.End.Event, "")
		case "checkpoint", "ready_for_review", "blocked":
			return s.finish(ctx, mission, attempt, fence, cp.End.Kind, "", "")
		default:
			return s.finish(ctx, mission, attempt, fence, "abandoned", "", "unknown end kind "+cp.End.Kind)
		}
	case claude.EndAbandoned:
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "no structured output")
	case claude.EndFailed:
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "result "+run.ResultSubtype)
	default:
		// No result event: the host and provider do not agree; the attempt
		// goes UNKNOWN through lease expiry, never to a relaunch (3.4).
		return nil
	}
}

// finish ends the attempt and, when the stream and exit agree, verifies it.
func (s *Scheduler) finish(ctx context.Context, mission domain.MissionID, attempt domain.AttemptID, fence int64, outcome, waitKey, reason string) error {
	cmd := domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: attempt, Fence: fence, Outcome: outcome, WaitKey: waitKey, Reason: reason}
	if s.gated() {
		defer s.releaseHolds(ctx, attempt)
	}
	if _, err := s.st.Execute(ctx, "fabd", domain.CommandID("finish:"+string(attempt)), cmd); err != nil {
		if errors.As(err, new(*store.Refusal)) {
			return nil
		}
		return err
	}
	if outcome == "abandoned" {
		return nil // FAILED_DEFINITE already; AC-3 relaunch policy is the daemon's next step
	}
	_, err := s.st.Execute(ctx, "fabd", domain.CommandID("verify:"+string(attempt)), domain.Command{Kind: domain.CmdReconcileAttempt, Mission: mission, Attempt: attempt, Fence: fence, Result: "VERIFIED"})
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
	for _, r := range rs {
		if r.State == store.ResHeld {
			if _, err := s.st.Release(ctx, r.ID); err != nil {
				fmt.Fprintln(os.Stderr, "fabd: release", r.ID+":", err)
			}
		}
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

// RequestJob inserts a REQUESTED job after checking the fence is live; the
// daemon starts it (review C1: the worker never reaches the user manager).
func RequestJob(ctx context.Context, st *store.Store, id string, mission domain.MissionID, attempt domain.AttemptID, fence int64, argv []string, rundir string) error {
	var cur int64
	var state string
	err := st.Pool().QueryRow(ctx, "SELECT fence, state FROM attempts WHERE id = $1", string(attempt)).Scan(&cur, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: attempt %s", domain.ErrNotFound, attempt)
	}
	if err != nil {
		return err
	}
	if cur != fence || !domain.AttemptState(state).Live() {
		return fmt.Errorf("%w: job for attempt %s at fence %d (holds %d, %s)", domain.ErrStaleFence, attempt, fence, cur, state)
	}
	a, _ := json.Marshal(argv)
	_, err = st.Pool().Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ($1, $2, $3, $4, $5, $6, 'REQUESTED') ON CONFLICT (id) DO NOTHING`,
		id, string(mission), string(attempt), fence, a, rundir)
	return err
}

// startRequestedJobs launches each REQUESTED job in its own unit (Linux) or
// as a detached process (macOS) from outside the sandbox, then marks it
// STARTED with the unit name, or FAILED_TO_START with the launcher output.
func (s *Scheduler) startRequestedJobs(ctx context.Context) (int, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT id, argv, rundir FROM jobs WHERE state = 'REQUESTED' ORDER BY started_at LIMIT 20`)
	if err != nil {
		return 0, err
	}
	type req struct {
		id, rundir string
		argv       []string
	}
	var reqs []req
	for rows.Next() {
		var r req
		var a []byte
		if err := rows.Scan(&r.id, &a, &r.rundir); err != nil {
			rows.Close()
			return 0, err
		}
		json.Unmarshal(a, &r.argv) //nolint:errcheck
		reqs = append(reqs, r)
	}
	rows.Close()
	for _, r := range reqs {
		if s.cfg.FabBin == "" || len(r.argv) == 0 {
			s.st.Pool().Exec(ctx, "UPDATE jobs SET state = 'FAILED_TO_START', log_tail = 'no fab binary or empty argv' WHERE id = $1", r.id) //nolint:errcheck
			continue
		}
		unit := "fab-job-" + r.id
		watch := append([]string{s.cfg.FabBin, "job", "watch", "--id", r.id, "--rundir", r.rundir, "--"}, r.argv...)
		launchErr := s.ln.StartJob(unit, watch, []string{"FABRIC_DSN=" + os.Getenv("FABRIC_DSN"), "FABRIC_HOME=" + filepath.Join(r.rundir, "fabric-home")})
		if launchErr != nil {
			s.st.Pool().Exec(ctx, "UPDATE jobs SET state = 'FAILED_TO_START', log_tail = $2 WHERE id = $1 AND state = 'REQUESTED'", r.id, launchErr.Error()) //nolint:errcheck
			continue
		}
		s.st.Pool().Exec(ctx, "UPDATE jobs SET state = 'STARTED', unit = $2 WHERE id = $1 AND state = 'REQUESTED'", r.id, unit) //nolint:errcheck
	}
	return len(reqs), nil
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
