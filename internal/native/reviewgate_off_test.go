package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
)

// With FABRIC_REVIEW_GATE unset (Options.ReviewGate false) a native run is the
// run of a build without the gate: no review DSN, no review rows, and the
// shell tool runs directly.
func TestReviewGateOffNeedsNoReviewDSNAndRunsToolsUngated(t *testing.T) {
	e := newEnv(t,
		fakeapi.Step{Events: fakeapi.ToolUse("", "", "toolu_off", "shell", `{"command":"touch off.marker"}`, 100, 20)},
		fakeapi.Step{Events: fakeapi.Text("finished", 120, 10)},
	)
	e.g.ShellAllow = []string{`^touch [A-Za-z0-9_.-]+$`}
	st := openNativeStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	o := opts(true)
	if o.ReviewGate || o.ReviewDSN != "" {
		t.Fatalf("fixture must be flag-off with no review DSN: %+v", o)
	}
	if _, err := native.Run(ctx, st, e.g, o); err != nil {
		t.Fatalf("flag-off run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.g.Worktree, "off.marker")); err != nil {
		t.Fatalf("flag-off tool did not run directly: %v", err)
	}
	var n int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM review_gate_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("flag-off run wrote %d review requests (err %v), want 0", n, err)
	}
}

// A grant that configures the gate must not silently run ungated.
func TestReviewGateOffRefusesAGrantThatConfiguresIt(t *testing.T) {
	e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 1, 1)})
	e.g.ReviewGate = native.ReviewGateConfig{Mode: "enforce"}
	st := openNativeStore(t)
	_, err := native.Run(context.Background(), st, e.g, opts(true))
	if err == nil || !strings.Contains(err.Error(), "FABRIC_REVIEW_GATE") {
		t.Fatalf("flag-off run of a gated grant: %v", err)
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Fatalf("refused run sent %d provider requests", n)
	}
}
