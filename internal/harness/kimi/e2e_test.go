package kimi_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
	"fabric/internal/harness/kimi"
)

// TestRealKimi drives the real kimi CLI through the adapter. Opt in with
// FABRIC_KIMI_E2E=1: it needs the subscription CLI installed and signed in,
// and spends subscription quota. The child gets PATH, HOME and a few
// identity variables only; nothing is read from the CLI's own credentials,
// and the test prints event kinds and counts, never event content.
func TestRealKimi(t *testing.T) {
	if os.Getenv("FABRIC_KIMI_E2E") != "1" {
		t.Skip("set FABRIC_KIMI_E2E=1 to run against the real kimi CLI")
	}
	bin, err := exec.LookPath("kimi")
	if err != nil {
		t.Fatalf("kimi is not on PATH: %v", err)
	}
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")}
	for _, k := range []string{"USER", "LOGNAME", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	dir := t.TempDir()
	for _, n := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := &kimi.Adapter{Launch: kimi.ExecLauncher(bin)}
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{
		SessionID: "ses_kimi_e2e", Dir: dir, Env: env,
		Prompt: "Count the files in the current directory and reply with just the number.",
	}, rec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		_ = s.Stop(context.Background())
		t.Fatalf("Wait: %v", err)
	}
	evs := rec.Snapshot()
	counts := map[harness.Kind]int{}
	for _, e := range evs {
		counts[e.Kind]++
	}
	t.Logf("final: outcome=%s exit_code=%d reason_len=%d", f.Outcome, f.ExitCode, len(f.Reason))
	t.Logf("events: %v", counts)
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatalf("CheckStream: %v", err)
	}
	if f.Outcome != harness.OutcomeDone {
		t.Fatalf("outcome %s (reason length %d)", f.Outcome, len(f.Reason))
	}
	if f.Reason != kimi.UnverifiedNotice {
		t.Fatalf("a done kimi run must carry the unverified notice")
	}
}
