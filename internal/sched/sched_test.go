//go:build linux

package sched

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"fabric/internal/connref"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
)

// AC-2 through the real Linux profile and the fake provider, since host jobs
// are disabled (goodqa#15082 F0): the fake's first run asks for a 5 s host
// job; `fab job run` is refused inside the sandbox, so the run ends blocked.
// Expected: exactly one run, no job row, no job unit, no wake. Opt-in: it
// needs FABRIC_AC2=1, FABRIC_TEST_DSN, FABRIC_FAB_BIN and the fake.
func TestAC2WaitWithoutPolling(t *testing.T) {
	if os.Getenv("FABRIC_AC2") == "" || runtime.GOOS != "linux" {
		t.Skip("set FABRIC_AC2=1 on Linux")
	}
	fab := os.Getenv("FABRIC_FAB_BIN")
	fake := os.Getenv("FABRIC_FAKE_PROVIDER")
	adminRef, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		t.Fatal("test admin reference unavailable")
	}
	defer adminRef.Close()
	dsn, err := adminRef.DSN()
	if err != nil || fab == "" || fake == "" || dsn == "" {
		t.Fatal("FABRIC_FAB_BIN, FABRIC_FAKE_PROVIDER and FABRIC_TEST_DSN or FABRIC_TEST_DSN_FILE are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A private database for this run.
	admin, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "fabric_ac2"
	admin.Pool().Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	var dbRef *connref.Ref
	defer func() {
		admin.Pool().Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
		if dbRef != nil {
			dbRef.Close()
		}
		admin.Close()
	}()
	dbRef, err = connref.ForTestDatabase(adminRef, name)
	if err != nil {
		t.Fatal("derive test database reference failed")
	}
	testDSN, err := dbRef.DSN()
	if err != nil {
		t.Fatal("test database reference unavailable")
	}
	t.Setenv("FABRIC_DSN", testDSN)
	st, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMissionSpec(ctx, store.MissionSpec{Provider: domain.ProviderClaudeCode, ID: "m_ac2", Name: "ac2", Ticket: "GoodQA goodqa#14791", Source: "file:" + "sched_test.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "test", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ac2"}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	schema := filepath.Join(work, "schema.json")
	os.WriteFile(schema, []byte(`{"type":"object"}`), 0o644) //nolint:errcheck
	port := 0
	if u, err := url.Parse(testDSN); err == nil {
		port, _ = strconv.Atoi(u.Port())
	}
	ln := &host.Linux{Self: fab, ReadOnly: []string{filepath.Dir(fab), filepath.Dir(fake)}, TCPPorts: []int{port}, User: os.Getenv("FABRIC_WORKER_USER")}
	if root := os.Getenv("FABRIC_WORKER_ROOT"); root != "" {
		work = filepath.Join(root, "ac2-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		os.MkdirAll(work, 0o775)            //nolint:errcheck
		os.Chmod(work, 0o775|os.ModeSetgid) //nolint:errcheck
	}
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5, MaxBudgetUSD: 0.01,
		Wall: time.Minute, WorkRoot: work, Backoff: 200 * time.Millisecond, MaxBackoff: time.Second, FabBin: fab}
	cfg.Launch.Binary = fake
	cfg.Launch.SchemaPath = schema
	s := New(st, cfg, ln)
	os.Setenv("FAB_BIN", fab)
	os.Setenv("PATH", filepath.Dir(fab)+":"+os.Getenv("PATH"))

	runCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	// Observe until the first run has finished, then give a second run time
	// to appear (none should).
	deadline := time.Now().Add(28 * time.Second)
	var finished int
	for time.Now().Before(deadline) {
		st.Pool().QueryRow(ctx, "SELECT count(*) FROM attempts WHERE mission_id = 'm_ac2' AND state = 'VERIFIED'").Scan(&finished) //nolint:errcheck
		if finished >= 1 {
			time.Sleep(3 * time.Second)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	stop()
	<-done

	var attempts, jobs, waitsOpen int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM attempts WHERE mission_id = 'm_ac2'").Scan(&attempts) //nolint:errcheck
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM jobs WHERE mission_id = 'm_ac2'").Scan(&jobs)         //nolint:errcheck
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM waits WHERE mission_id = 'm_ac2'").Scan(&waitsOpen)   //nolint:errcheck
	events, _ := st.Events(ctx, "m_ac2")
	var seq []string
	for _, e := range events {
		seq = append(seq, string(e.Kind)+"/"+e.Outcome)
	}
	t.Logf("attempts=%d exited_jobs=%d waits_open=%d launched=%d waits_resumed=%d jobs_woken=%d events=%v", attempts, jobs, waitsOpen, s.Launched, s.WaitsResumed, s.JobsWoken, seq)
	if attempts != 1 || jobs != 0 || waitsOpen != 0 || s.Launched != 1 || s.WaitsResumed != 0 || s.JobsWoken != 0 || finished != 1 {
		t.Fatalf("AC-2 failed: attempts=%d jobs=%d waits_open=%d launched=%d resumed=%d woken=%d verified=%d", attempts, jobs, waitsOpen, s.Launched, s.WaitsResumed, s.JobsWoken, finished)
	}
	// The only run ended blocked (its job was refused); it never waited.
	blocked := 0
	for _, x := range seq {
		if x == "ATTEMPT_FINISHED/wait" || x == "MISSION_RESUMED/" {
			t.Fatalf("a refused job still produced %s: %v", x, seq)
		}
		if x == "ATTEMPT_FINISHED/blocked" {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("want one ATTEMPT_FINISHED/blocked, got %v", seq)
	}
	// Cleanup: no job unit remains.
	out, _ := exec.Command("systemctl", "--user", "list-units", "fab-job-*", "--no-legend").Output()
	if len(out) != 0 {
		t.Fatalf("job units still present:\n%s", out)
	}
}
