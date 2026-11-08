//go:build linux

package sched

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/probe"
	"fabric/internal/store"
)

// Live gated mission on t2-dev (goodqa#14922, after G1, G2 and #14929). Opt-in
// with FABRIC_LIVE_GATE=1. It needs a migrated database with the roles
// applied (FABRIC_DSN as the app role, FABRIC_RUNNER_DSN as the runner
// role, both real logins), FABRIC_FAB_BIN, FABRIC_FAKE_PROVIDER (a fake
// that runs for a while), FABRIC_WORKER_USER and FABRIC_WORKER_ROOT.
//
// A child process runs the gated scheduler with capacity 1 and two
// missions: A runs as fab-worker with its hold and B waits. The child is
// then SIGKILLed mid-run. This parent is the restarted scheduler with the
// real cgroup readback. While A's unit lives, A stays UNKNOWN and keeps its
// slot. Once the unit is collected, A is quarantined, the slot frees, and B
// runs.

func liveEnv(t *testing.T) (app, runner, fab, fake, worker, root string) {
	if os.Getenv("FABRIC_LIVE_GATE") == "" {
		t.Skip("set FABRIC_LIVE_GATE=1 on t2-dev")
	}
	app, runner = os.Getenv("FABRIC_DSN"), os.Getenv("FABRIC_RUNNER_DSN")
	fab, fake = os.Getenv("FABRIC_FAB_BIN"), os.Getenv("FABRIC_FAKE_PROVIDER")
	worker, root = os.Getenv("FABRIC_WORKER_USER"), os.Getenv("FABRIC_WORKER_ROOT")
	if app == "" || runner == "" || fab == "" || fake == "" || worker == "" || root == "" {
		t.Fatal("FABRIC_DSN, FABRIC_RUNNER_DSN, FABRIC_FAB_BIN, FABRIC_FAKE_PROVIDER, FABRIC_WORKER_USER and FABRIC_WORKER_ROOT are required")
	}
	return
}

func liveSched(t *testing.T, st *store.Store, tag string) (*Scheduler, Config) {
	app, runner, fab, fake, worker, root := liveEnv(t)
	port := 0
	if u, err := url.Parse(app); err == nil {
		port, _ = strconv.Atoi(u.Port())
	}
	work := filepath.Join(root, "live-"+tag)
	os.MkdirAll(work, 0o775)            //nolint:errcheck
	os.Chmod(work, 0o775|os.ModeSetgid) //nolint:errcheck
	schema := filepath.Join(work, "schema.json")
	os.WriteFile(schema, []byte(`{"type":"object"}`), 0o644) //nolint:errcheck
	ln := &host.Linux{Self: fab, ReadOnly: []string{filepath.Dir(fab), filepath.Dir(fake)}, TCPPorts: []int{port}, User: worker}
	cfg := Config{Host: "h_t2live", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "live", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: 2 * time.Minute, WorkRoot: work, Backoff: 200 * time.Millisecond, MaxBackoff: time.Second, FabBin: fab,
		Holds: []store.HoldItem{{Resource: "slot.live", Unit: "slots", Amount: 1}}, RunnerDSN: runner}
	cfg.Launch.Binary = fake
	cfg.Launch.SchemaPath = schema
	os.Setenv("FAB_BIN", fab)
	os.Setenv("PATH", filepath.Dir(fab)+":"+os.Getenv("PATH"))
	s := New(st, cfg, ln)
	return s, cfg
}

// TestLiveGateChild is the first scheduler; the parent kills it.
func TestLiveGateChild(t *testing.T) {
	tag := os.Getenv("FABRIC_LIVE_TAG")
	if tag == "" {
		t.Skip("child of TestLiveGatedRestart")
	}
	app, _, _, _, _, _ := liveEnv(t)
	ctx := context.Background()
	st, err := store.Open(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	st.Lease = 15 * time.Second
	s, _ := liveSched(t, st, tag)
	s.Run(ctx) //nolint:errcheck // runs until SIGKILL
}

func TestLiveGatedRestart(t *testing.T) {
	app, _, _, _, worker, _ := liveEnv(t)
	ctx := context.Background()
	st, err := store.Open(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Lease = 15 * time.Second
	tag := strconv.FormatInt(time.Now().Unix(), 36)
	a, b := domain.MissionID("m_lga"+tag), domain.MissionID("m_lgb"+tag)
	if err := st.SetCapacity(ctx, "slot.live", "slots", 1); err != nil {
		t.Fatal(err)
	}
	uid := 0
	if u, err := exec.Command("id", "-u", worker).Output(); err == nil {
		uid, _ = strconv.Atoi(string(u[:len(u)-1]))
	}
	pr := probe.Cgroup{Root: "/sys/fs/cgroup", UID: uid}
	// An earlier run's last attempt is left UNKNOWN with its hold: the
	// restarted scheduler reads it back and frees the slot first.
	pre, _ := liveSched(t, st, tag+"p")
	pre.SetProber(pr)
	for end := time.Now().Add(90 * time.Second); ; time.Sleep(time.Second) {
		st.ExpireLeases(ctx, 10) //nolint:errcheck
		if _, err := pre.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if _, h, _ := st.Capacity(ctx, "slot.live"); h == 0 {
			break
		} else if time.Now().After(end) {
			t.Fatalf("slot.live still holds %d after reading back earlier runs", h)
		}
	}
	for _, m := range []domain.MissionID{a, b} {
		if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#14922", "fabric", "main"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(m)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(os.Args[0], "-test.run", "^TestLiveGateChild$", "-test.v")
	child.Env = append(os.Environ(), "FABRIC_LIVE_TAG="+tag)
	logf, _ := os.Create(filepath.Join(os.Getenv("FABRIC_WORKER_ROOT"), "live-"+tag+".child.log"))
	child.Stdout, child.Stderr = logf, logf
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	attemptA := domain.AttemptID("r_lga" + tag + "_1")
	state := func(id domain.AttemptID) string {
		var s string
		st.Pool().QueryRow(ctx, "SELECT state FROM attempts WHERE id = $1", string(id)).Scan(&s) //nolint:errcheck
		return s
	}
	waitFor := func(what string, d time.Duration, ok func() bool) {
		t.Helper()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if ok() {
				t.Logf("%s after %s", what, time.Since(end.Add(-d)).Round(time.Second))
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		child.Process.Kill() //nolint:errcheck
		t.Fatalf("timed out waiting for %s (A is %q)", what, state(attemptA))
	}
	waitFor("A running under its hold", 30*time.Second, func() bool { return state(attemptA) == "RUNNING" })
	if _, h, _ := st.Capacity(ctx, "slot.live"); h != 1 {
		t.Fatalf("held %d while A runs, want 1", h)
	}
	time.Sleep(3 * time.Second) // B gets a few ticks to wait
	if n, _ := counts(t, st, b); n != 0 {
		t.Fatalf("B has %d attempts while the slot is held", n)
	}
	// The first scheduler dies mid-run: no settle, no release, no heartbeat.
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	child.Wait() //nolint:errcheck
	s, _ := liveSched(t, st, tag+"b")
	s.SetProber(pr)
	tick := func() {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sawLiveUnknown := false
	waitFor("A quarantined after its unit ended", 3*time.Minute, func() bool {
		tick()
		alive, known := pr.Alive(attemptA)
		if state(attemptA) == "UNKNOWN" && alive && known {
			sawLiveUnknown = true
		}
		return state(attemptA) == "QUARANTINED"
	})
	if !sawLiveUnknown {
		t.Log("note: A's unit had already ended when its lease lapsed; the alive-and-UNKNOWN phase was not observed live")
	}
	waitFor("B started once the slot freed", 60*time.Second, func() bool {
		tick()
		n, _ := counts(t, st, b)
		return n > 0 && s.Launched > 0
	})
	// B's run asks for a host job as the runner role. Host jobs are disabled
	// (goodqa#15082 F0): the request is refused inside the sandbox, no job
	// row is written, and B ends blocked instead of waiting on a job.
	attemptB := domain.AttemptID("r_lgb" + tag + "_1")
	waitFor("B's host job refused and B ended", 150*time.Second, func() bool {
		tick()
		return state(attemptB) == "VERIFIED"
	})
	var jobsB int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM jobs WHERE attempt_id = $1", string(attemptB)).Scan(&jobsB) //nolint:errcheck
	if jobsB != 0 {
		t.Fatalf("B's refused host job left %d job row(s)", jobsB)
	}
	fmt.Printf("live gated restart: tag=%s launched_after_restart=%d capacity_waits=%d saw_live_unknown=%v\n", tag, s.Launched, s.CapacityWaits, sawLiveUnknown)
}
