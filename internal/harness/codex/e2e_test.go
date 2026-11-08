package codex_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
	"fabric/internal/harness/harnesstest"
)

// TestRealCodex drives the real `codex app-server` through the adapter. It is
// opt in (FABRIC_CODEX_E2E=1) because it needs an installed, already signed-in
// codex and spends model tokens. The process gets only PATH and HOME from the
// caller; nothing is read or printed from codex's own credentials, and the
// test prints event kinds and counts, never event content.
func TestRealCodex(t *testing.T) {
	if os.Getenv("FABRIC_CODEX_E2E") != "1" {
		t.Skip("set FABRIC_CODEX_E2E=1 to run against the real codex")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("codex is not installed on PATH: %v", err)
	}
	a := &codex.Adapter{
		Command:        []string{bin, "app-server"},
		BaseEnv:        map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")},
		RequestTimeout: 90 * time.Second,
		KillGrace:      5 * time.Second,
	}
	dir := t.TempDir()
	rec := &harnesstest.Recorder{}
	s, err := a.Start(context.Background(), harness.StartSpec{
		SessionID: "ses_codex_e2e", Dir: dir,
		Prompt: "Create a file hello.txt containing the word fabric, then stop",
	}, rec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f, err := s.Wait(ctx)
	if err != nil {
		_ = s.Stop(context.Background())
		t.Fatalf("Wait: %v (events so far: %s)", err, summary(rec.Snapshot()))
	}
	evs := rec.Snapshot()
	t.Logf("final: outcome=%s exit_code=%d reason_len=%d", f.Outcome, f.ExitCode, len(f.Reason))
	t.Logf("events: %s", summary(evs))
	if err := harnesstest.CheckStream(evs); err != nil {
		t.Fatalf("CheckStream: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	t.Logf("hello.txt: exists=%v bytes=%d contains_fabric=%v", err == nil, len(b), strings.Contains(string(b), "fabric"))
	if f.Outcome != harness.OutcomeDone {
		reason := f.Reason
		if len(reason) > 300 {
			reason = reason[:300]
		}
		t.Fatalf("outcome %s, want done; reason (failure diagnostics, first 300 bytes): %s", f.Outcome, reason)
	}
	if err != nil || !strings.Contains(string(b), "fabric") {
		t.Fatal("the agent did not create hello.txt containing fabric")
	}
}

// summary counts events by kind, and unknown events by their classification
// (a method name or short reason, never harness content).
func summary(evs []harness.Event) string {
	kinds := map[harness.Kind]int{}
	whys := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
		if e.Kind == harness.KindUnknown {
			var u harness.Unknown
			if err := json.Unmarshal(e.Payload, &u); err == nil {
				whys[u.Why]++
			}
		}
	}
	var parts []string
	for k, n := range kinds {
		parts = append(parts, string(k)+"="+strconv.Itoa(n))
	}
	sort.Strings(parts)
	var ws []string
	for w, n := range whys {
		ws = append(ws, strconv.Itoa(n)+"x "+w)
	}
	sort.Strings(ws)
	return strings.Join(parts, " ") + " | unknown: " + strings.Join(ws, "; ")
}
