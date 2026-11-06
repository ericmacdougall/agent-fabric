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
	Launch       claude.Launch
	Backoff      time.Duration // backstop timer; doubles up to MaxBackoff
	MaxBackoff   time.Duration
}

// Scheduler owns one host's dispatch loop.
type Scheduler struct {
	st  *store.Store
	cfg Config
	ln  Launcher
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
	return &Scheduler{st: st, cfg: cfg, ln: ln}
}

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
		return false, err
	}
	s.Expired += len(exp)
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
	resp, err := s.st.Execute(ctx, "fabd", domain.CommandID("start:"+string(attempt)), domain.Command{Kind: domain.CmdStartAttempt, Mission: mission, Attempt: attempt, Host: s.cfg.Host, Role: "writer"})
	if err != nil {
		var ref *store.Refusal
		if errors.As(err, &ref) {
			return nil // someone else moved the mission; the next tick sees the new state
		}
		return err
	}
	fence := resp.Events[0].Fence
	s.Launched++
	s.bump(ctx, "runs_launched_total")

	worktree := filepath.Join(s.cfg.WorkRoot, "wt", string(mission))
	runDir := filepath.Join(s.cfg.WorkRoot, "run", string(attempt))
	for _, d := range []string{worktree, runDir, filepath.Join(runDir, "jobs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	events, err := s.st.Events(ctx, mission)
	if err != nil {
		return err
	}
	proj, err := projection.Build(m, events, projection.DefaultCapTokens, "continue the mission; end with the checkpoint schema")
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
	env = append(env, "FAB_RUN_DIR="+runDir, "FAB_WORKTREE="+worktree, "FABRIC_DSN="+os.Getenv("FABRIC_DSN"), "FABRIC_HOME="+os.Getenv("FABRIC_HOME"))
	out, err := os.Create(filepath.Join(runDir, "stream.jsonl"))
	if err != nil {
		return err
	}
	errf, _ := os.Create(filepath.Join(runDir, "stderr.log"))
	h, err := s.ln.Start(g, argv, env, out, errf)
	if err != nil {
		out.Close()
		return s.finish(ctx, mission, attempt, fence, "abandoned", "", "start failed: "+err.Error())
	}
	if _, err := s.st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(attempt)), domain.Command{Kind: domain.CmdAckStart, Mission: mission, Attempt: attempt, Fence: fence}); err != nil {
		return err
	}
	code, _ := s.ln.Wait(h)
	out.Close()
	if errf != nil {
		errf.Close()
	}
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

// RegisterJob inserts a STARTED job after checking the fence is live.
func RegisterJob(ctx context.Context, st *store.Store, id string, mission domain.MissionID, attempt domain.AttemptID, fence int64, argv []string, unit string) error {
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
	_, err = st.Pool().Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, unit) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
		id, string(mission), string(attempt), fence, a, unit)
	return err
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
