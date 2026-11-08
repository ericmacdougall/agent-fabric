package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fabric/internal/projection"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// These tests drive runOnce with a fake launcher (no units, no provider) and
// a real Postgres. They pin the launch-ownership rules from the Kimi P3
// reconciliation (S1-S4): one owner per attempt, ACK before the physical
// start, counters after it, and an ambiguous start never treated as a
// definite abandonment.

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "sch")) }

type fakeLauncher struct {
	st       *store.Store
	starts   int32
	startErr error
	mu       sync.Mutex
	stateAt  []string // the attempt row state at each Start call
	argvs    [][]string
	envs     [][]string
	block    chan struct{}
}

func (f *fakeLauncher) Start(g host.Grant, argv, env []string, stdout, stderr *os.File) (host.Handle, error) {
	atomic.AddInt32(&f.starts, 1)
	var st string
	f.st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = $1`, string(g.Run)).Scan(&st) //nolint:errcheck
	f.mu.Lock()
	f.stateAt = append(f.stateAt, st)
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	f.envs = append(f.envs, append([]string(nil), env...))
	f.mu.Unlock()
	return host.Handle{Run: g.Run}, f.startErr
}

// Wait never returns during the test: the run stays live, so settle never
// races the assertions.
func (f *fakeLauncher) Wait(host.Handle) (int, error)             { <-f.block; return 0, nil }
func (f *fakeLauncher) StartJob(string, []string, []string) error { return nil }

func ownerSetup(t *testing.T, id domain.MissionID) (*store.Store, *fakeLauncher, Config) {
	t.Helper()
	st := storetest.Open(t)
	ctx := context.Background()
	if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `DELETE FROM sched_counters`); err != nil { // not covered by Truncate
		t.Fatal(err)
	}
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir(), RunnerDSN: storetest.RunnerDSN(t, st)}
	cfg.Launch.Binary = "/bin/true"
	return st, ln, cfg
}

// S1: two schedulers that read the same mission version race one START.
// The replayed START must not confer launch ownership: one physical start.
func TestConcurrentRunOnceHasSingleLaunchOwner(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_own1")
	const n = 4
	var mu sync.Mutex
	arrived := 0
	wait := func() { // all n read the mission before any START
		mu.Lock()
		arrived++
		mu.Unlock()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
			mu.Lock()
			all := arrived >= n
			mu.Unlock()
			if all {
				return
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		s := New(st, cfg, ln)
		s.afterRead = wait
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.runOnce(context.Background(), "m_own1"); err != nil {
				t.Errorf("runOnce: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&ln.starts); got != 1 {
		t.Fatalf("%d physical starts for one attempt, want 1 (a replayed START launched)", got)
	}
}

// S2: ACK_START is durable before the physical start, so the pre-launch
// ownership boundary is recorded first and a start never precedes it.
func TestAckStartPersistedBeforeLaunch(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_own2")
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_own2"); err != nil {
		t.Fatal(err)
	}
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if len(ln.stateAt) != 1 || ln.stateAt[0] != string(domain.AttemptRunning) {
		t.Fatalf("attempt state seen by Start: %v, want [%s] (ACK must commit before the launch)", ln.stateAt, domain.AttemptRunning)
	}
}

func launchedCounter(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT COALESCE((SELECT value FROM sched_counters WHERE name = 'runs_launched_total'), 0)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// S3: launch counters count physical starts, not selected candidates.
func TestLaunchCountersOnlyAfterPhysicalStart(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_own3")
	ln.startErr = errors.New("unit could not be created")
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_own3"); err != nil {
		t.Fatal(err)
	}
	if s.Launched != 0 || launchedCounter(t, st) != 0 {
		t.Fatalf("a failed start counted as a launch: Launched=%d runs_launched_total=%d", s.Launched, launchedCounter(t, st))
	}
	// A start that succeeds counts once.
	st2, ln2, cfg2 := ownerSetup(t, "m_own3b")
	s2 := New(st2, cfg2, ln2)
	if err := s2.runOnce(context.Background(), "m_own3b"); err != nil {
		t.Fatal(err)
	}
	if s2.Launched != 1 || launchedCounter(t, st2) != 1 {
		t.Fatalf("a successful start: Launched=%d runs_launched_total=%d, want 1/1", s2.Launched, launchedCounter(t, st2))
	}
}

// S4: a Start error is ambiguous (systemd-run can fail after the unit
// exists). The attempt keeps its lease, which expires into UNKNOWN for
// reconciliation; it is never a definite abandonment that lets the next
// pass launch a second worker beside a possibly live one.
func TestAmbiguousStartErrorKeepsLease(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_own4")
	ln.startErr = errors.New("systemd-run: connection reset after unit creation")
	s := New(st, cfg, ln)
	ctx := context.Background()
	if err := s.runOnce(ctx, "m_own4"); err != nil {
		t.Fatal(err)
	}
	var state string
	var leased bool
	if err := st.Pool().QueryRow(ctx, `SELECT state, COALESCE(lease_until > now(), false) FROM attempts WHERE mission_id = 'm_own4'`).Scan(&state, &leased); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.AttemptRunning) || !leased {
		t.Fatalf("after an ambiguous start: attempt %s leased=%v, want RUNNING with its lease", state, leased)
	}
	if _, err := s.dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&ln.starts); got != 1 {
		t.Fatalf("%d starts: the next pass launched beside a possibly live worker", got)
	}
}

// Kimi projection analysis (defect 2), checked against fabric: runOnce built
// the launch projection from the mission read before START and the events
// read after it, so the checkpoint said fence 0 with no live attempt while
// the window held that attempt's start. The checkpoint and the window now
// come from one verified snapshot taken after START.
func TestRunOnceProjectionUsesPostStartSnapshot(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_own6")
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_own6"); err != nil {
		t.Fatal(err)
	}
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if len(ln.argvs) != 1 {
		t.Fatalf("%d starts, want 1", len(ln.argvs))
	}
	var proj projection.Projection
	for i, a := range ln.argvs[0] {
		if a == "-p" && i+1 < len(ln.argvs[0]) {
			if err := json.Unmarshal([]byte(ln.argvs[0][i+1]), &proj); err != nil {
				t.Fatal(err)
			}
		}
	}
	cp := proj.Checkpoint
	if cp.Version.AttemptFence != 1 || len(cp.LiveRuns) != 1 || cp.LiveRuns[0] != "r_own6_1" {
		t.Fatalf("checkpoint is not the post-start state: fence %d, live %v", cp.Version.AttemptFence, cp.LiveRuns)
	}
	if n := len(proj.Window); n < 2 || !strings.Contains(proj.Window[n-1], string(domain.EvAttemptStarted)) {
		t.Fatalf("window does not end with the attempt start: %v", proj.Window)
	}
}
