package sched

import (
	"context"
	"os"
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
	block    chan struct{}
}

func (f *fakeLauncher) Start(g host.Grant, argv, env []string, stdout, stderr *os.File) (host.Handle, error) {
	atomic.AddInt32(&f.starts, 1)
	var st string
	f.st.Pool().QueryRow(context.Background(), `SELECT state FROM attempts WHERE id = $1`, string(g.Run)).Scan(&st) //nolint:errcheck
	f.mu.Lock()
	f.stateAt = append(f.stateAt, st)
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
	if err := st.CreateMission(ctx, id, "GoodQA goodqa#14922", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
		t.Fatal(err)
	}
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir()}
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
