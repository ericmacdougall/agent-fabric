//go:build argvgolden

package sched

// Capture helper for goodqa#15373 AC-1, run ONLY in a bd56178 tree:
//
//	cp testdata/argv/_capture/zz_argv_capture_test.go internal/sched/
//	go test -tags argvgolden -run TestCaptureArgvGolden ./internal/sched/
//
// It launches one mission through bd56178's runOnce with a recording
// launcher and writes testdata/argv/golden.json: the argv, env and grant
// passed to Start, normalized by argvNormalize (argv_golden_test.go has the
// same function).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"fabric/internal/host"
)

type recLauncher struct {
	g    host.Grant
	argv []string
	env  []string
	n    int
	stop chan struct{}
}

func (r *recLauncher) Start(g host.Grant, argv, env []string, stdout, stderr *os.File) (host.Handle, error) {
	r.n++
	r.g, r.argv, r.env = g, append([]string(nil), argv...), append([]string(nil), env...)
	return host.Handle{Run: g.Run}, nil
}
func (r *recLauncher) Wait(host.Handle) (int, error) { <-r.stop; return 0, nil }

func argvNormalize(work, token, runnerDSN string, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		if runnerDSN != "" {
			x = strings.ReplaceAll(x, runnerDSN, "$RUNNER_DSN")
		}
		x = strings.ReplaceAll(x, token, "$TOKEN")
		out[i] = strings.ReplaceAll(x, work, "$WORK")
	}
	return out
}

func TestCaptureArgvGolden(t *testing.T) {
	st, _, cfg := ownerSetup(t, "m_argvg")
	cfg.Launch.Binary = "/bin/true"
	ln := &recLauncher{stop: make(chan struct{})}
	t.Cleanup(func() { close(ln.stop) })
	s := New(st, cfg, ln)
	if err := s.runOnce(context.Background(), "m_argvg"); err != nil || ln.n != 1 {
		t.Fatalf("runOnce: %v, starts %d", err, ln.n)
	}
	argv := append([]string(nil), ln.argv...)
	if len(argv) < 3 || argv[1] != "-p" {
		t.Fatalf("unexpected argv %q", argv)
	}
	argv[2] = "$PROMPT"
	g := ln.g
	tok := g.Token
	g.Token, g.Worktree, g.RunDir = "$TOKEN", strings.ReplaceAll(g.Worktree, cfg.WorkRoot, "$WORK"), strings.ReplaceAll(g.RunDir, cfg.WorkRoot, "$WORK")
	out, _ := json.MarshalIndent(map[string]any{
		"argv":  argvNormalize(cfg.WorkRoot, tok, cfg.RunnerDSN, argv),
		"env":   argvNormalize(cfg.WorkRoot, tok, cfg.RunnerDSN, ln.env),
		"grant": g,
	}, "", "  ")
	os.MkdirAll("testdata/argv", 0o755)
	if err := os.WriteFile("testdata/argv/golden.json", append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", out)
}
