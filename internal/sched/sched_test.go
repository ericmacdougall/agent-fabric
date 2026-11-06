//go:build linux

package sched

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
)

// AC-2 through the real Linux profile and the fake provider: a mission whose
// first run starts a 5 s host job and waits. Expected: exactly two runs, the
// first ending in wait, a job exit that wakes the mission, the second ending
// blocked (the fake has nothing more to do), and no run while the job ran. Opt-in: it launches
// transient units and needs FABRIC_TEST_DSN, FABRIC_FAB_BIN and the fake.
func TestAC2WaitWithoutPolling(t *testing.T) {
	if os.Getenv("FABRIC_AC2") == "" || runtime.GOOS != "linux" {
		t.Skip("set FABRIC_AC2=1 on Linux")
	}
	fab := os.Getenv("FABRIC_FAB_BIN")
	fake := os.Getenv("FABRIC_FAKE_PROVIDER")
	dsn := os.Getenv("FABRIC_TEST_DSN")
	if fab == "" || fake == "" || dsn == "" {
		t.Fatal("FABRIC_FAB_BIN, FABRIC_FAKE_PROVIDER and FABRIC_TEST_DSN are required")
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
	admin.Close()
	testDSN := replaceDB(dsn, name)
	os.Setenv("FABRIC_DSN", testDSN)
	st, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMissionSpec(ctx, store.MissionSpec{ID: "m_ac2", Name: "ac2", Ticket: "GoodQA goodqa#14791", Source: "file:" + "sched_test.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "test", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ac2"}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	schema := filepath.Join(work, "schema.json")
	os.WriteFile(schema, []byte(`{"type":"object"}`), 0o644) //nolint:errcheck
	ln := &host.Linux{Self: fab, ExtraRW: []string{filepath.Join(os.Getenv("HOME"), ".cache")}}
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5, MaxBudgetUSD: 0.01,
		Wall: time.Minute, WorkRoot: work, Backoff: 200 * time.Millisecond, MaxBackoff: time.Second}
	cfg.Launch.Binary = fake
	cfg.Launch.SchemaPath = schema
	s := New(st, cfg, ln)
	os.Setenv("FAB_BIN", fab)
	os.Setenv("PATH", filepath.Dir(fab)+":"+os.Getenv("PATH"))

	runCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	// Observe until the mission is back to ADMITTED after its second run or time runs out.
	deadline := time.Now().Add(28 * time.Second)
	var finished int
	for time.Now().Before(deadline) {
		st.Pool().QueryRow(ctx, "SELECT count(*) FROM attempts WHERE mission_id = 'm_ac2' AND state = 'VERIFIED'").Scan(&finished) //nolint:errcheck
		if finished >= 2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	stop()
	<-done

	var attempts, jobs, waitsOpen int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM attempts WHERE mission_id = 'm_ac2'").Scan(&attempts)                                //nolint:errcheck
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM jobs WHERE mission_id = 'm_ac2' AND state = 'EXITED' AND exit_code = 0").Scan(&jobs) //nolint:errcheck
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM waits WHERE mission_id = 'm_ac2'").Scan(&waitsOpen)                                  //nolint:errcheck
	events, _ := st.Events(ctx, "m_ac2")
	var seq []string
	for _, e := range events {
		seq = append(seq, string(e.Kind)+"/"+e.Outcome)
	}
	t.Logf("attempts=%d exited_jobs=%d waits_open=%d launched=%d waits_resumed=%d jobs_woken=%d events=%v", attempts, jobs, waitsOpen, s.Launched, s.WaitsResumed, s.JobsWoken, seq)
	if attempts != 2 || jobs != 1 || waitsOpen != 0 || s.Launched != 2 || s.WaitsResumed != 1 || s.JobsWoken != 1 || finished != 2 {
		t.Fatalf("AC-2 failed: attempts=%d jobs=%d waits_open=%d launched=%d resumed=%d woken=%d verified=%d", attempts, jobs, waitsOpen, s.Launched, s.WaitsResumed, s.JobsWoken, finished)
	}
	// The first run ended in wait, the second in checkpoint, in that order, with a resume between.
	want := []string{"ATTEMPT_FINISHED/wait", "MISSION_RESUMED/", "ATTEMPT_FINISHED/blocked"}
	i := 0
	for _, x := range seq {
		if i < len(want) && x == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("event order: want %v in order, got %v", want, seq)
	}
	// Cleanup: no job unit remains.
	out, _ := exec.Command("systemctl", "--user", "list-units", "fab-job-*", "--no-legend").Output()
	if len(out) != 0 {
		t.Fatalf("job units still present:\n%s", out)
	}
}

func replaceDB(dsn, name string) string {
	i := len(dsn) - 1
	for i >= 0 && dsn[i] != '/' {
		i--
	}
	rest := dsn[i+1:]
	q := ""
	if j := indexByte(rest, '?'); j >= 0 {
		q = rest[j:]
	}
	return dsn[:i+1] + name + q
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
