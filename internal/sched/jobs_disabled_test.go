package sched

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"fabric/internal/domain"
)

// countingJobs records every host-job launch the scheduler attempts.
type countingJobs struct {
	*fakeLauncher
	jobStarts int32
}

func (c *countingJobs) StartJob(string, []string, []string) error {
	atomic.AddInt32(&c.jobStarts, 1)
	return nil
}

// goodqa#15082 F0: a REQUESTED job row (here written directly, as a runner
// could before 0025) is never launched. A row of this host's attempt is
// retired to FAILED_TO_START; a row of another host's attempt is left alone.
func TestRequestedJobNeverLaunched(t *testing.T) {
	st, ln, cfg := ownerSetup(t, "m_job1")
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "escaped")
	cfg.FabBin = "/bin/true" // before the fix, a configured binary is what made it launch
	if _, err := st.Execute(ctx, "fabd", "start:r_job1", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_job1", Attempt: "r_job1", Host: cfg.Host, Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMission(ctx, "m_job2", domain.ProviderClaudeCode, "GoodQA goodqa#15082", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", "admit-m_job2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_job2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", "start:r_job2", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_job2", Attempt: "r_job2", Host: "h_other", Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) SELECT 'j_own', 'm_job1', 'r_job1', fence, $1::jsonb, '/x', 'REQUESTED' FROM attempts WHERE id = 'r_job1'`,
		`INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) SELECT 'j_other', 'm_job2', 'r_job2', fence, $1::jsonb, '/x', 'REQUESTED' FROM attempts WHERE id = 'r_job2'`,
	} {
		if _, err := st.Pool().Exec(ctx, q, `["touch","`+marker+`"]`); err != nil {
			t.Fatal(err)
		}
	}
	cj := &countingJobs{fakeLauncher: ln}
	s := New(st, cfg, cj)
	for i := 0; i < 3; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt32(&cj.jobStarts); n != 0 {
		t.Errorf("the scheduler launched %d host job(s); want none", n)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the job argv ran")
	}
	state := func(id string) string {
		var s string
		st.Pool().QueryRow(ctx, "SELECT state FROM jobs WHERE id = $1", id).Scan(&s) //nolint:errcheck
		return s
	}
	if got := state("j_own"); got != "FAILED_TO_START" {
		t.Errorf("own-host REQUESTED job: state %q, want FAILED_TO_START", got)
	}
	if got := state("j_other"); got != "REQUESTED" {
		t.Errorf("another host's REQUESTED job: state %q, want it left REQUESTED", got)
	}
}

// RequestJob inserts nothing: host jobs are disabled before any write.
func TestRequestJobRefused(t *testing.T) {
	st, _, cfg := ownerSetup(t, "m_job3")
	ctx := context.Background()
	resp, err := st.Execute(ctx, "fabd", "start:r_job3", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: "m_job3", Attempt: "r_job3", Host: cfg.Host, Role: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	err = RequestJob(ctx, st, "j_req", "m_job3", "r_job3", resp.Events[0].Fence, []string{"sh", "-c", "id"}, t.TempDir())
	var n int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&n) //nolint:errcheck
	if err == nil || n != 0 {
		t.Fatalf("RequestJob: err %v, %d job row(s); want a refusal and none", err, n)
	}
}
