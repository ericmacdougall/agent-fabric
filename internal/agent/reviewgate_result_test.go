package agent_test

import (
	"context"
	"strings"
	"testing"

	"fabric/internal/reviewgate"
)

func TestReviewGateCrashReplayUsesRedactedBoundedDurableStdout(t *testing.T) {
	r := newRig(t,
		toolStep("", "", "toolu_review", "shell", reviewToolInput),
		endStep("finished"),
	)
	realTool := &fakeTool{name: "shell", side: true, mu: &r.mu, runs: &r.runs}
	realTool.out = func(string) string {
		return "stdout " + canary + " " + strings.Repeat("z", reviewgate.MaxDurableResultBytes+128)
	}
	gatedShell(t, r, reviewgate.Enforce, true, realTool)
	r.crashOnce("before_finish_tool") // terminal gate commit succeeded; process died before agent checkpoint
	if _, err, died := r.life(); err != nil || died == nil {
		t.Fatalf("crash injection: err=%v died=%+v", err, died)
	}
	if len(r.runs) != 1 {
		t.Fatalf("test setup executed %d times", len(r.runs))
	}
	hash := onlyReviewHash(t, r)
	var stored []byte
	var truncated bool
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT output, truncated FROM review_gate_results WHERE request_hash = $1`, hash).
		Scan(&stored, &truncated); err != nil {
		t.Fatal(err)
	}
	if len(stored) != reviewgate.MaxDurableResultBytes || !truncated || strings.Contains(string(stored), canary) || !strings.Contains(string(stored), "[REDACTED:") {
		t.Fatalf("durable stdout was not bounded and redacted: len=%d truncated=%v", len(stored), truncated)
	}

	// r.loop constructs a fresh GatedTool, so no executor result cache survives.
	runReviewLoop(t, r, "done")
	if len(r.runs) != 1 || len(r.srv.Requests()) != 2 {
		t.Fatalf("durable replay repeated work: runs=%d requests=%d", len(r.runs), len(r.srv.Requests()))
	}
	secondRequest := string(r.srv.Requests()[1].Body)
	if strings.Contains(secondRequest, canary) || strings.Contains(secondRequest, "confirmed_replay") || !strings.Contains(secondRequest, "[REDACTED:") || !strings.Contains(secondRequest, "[output truncated]") {
		t.Fatal("cross-process replay did not return the durable redacted stdout")
	}
}
