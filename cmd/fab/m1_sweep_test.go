package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
)

// fakeSweepManager records what the sweep launcher hands the worker manager.
type fakeSweepManager struct {
	calls   []string
	grant   host.Grant
	argv    []string
	env     []string
	props   map[string]string
	stdin   []byte
	report  string
	startEr error
	stopRC  host.StopReceipt
}

func (f *fakeSweepManager) StartIOWithProperties(g host.Grant, argv, env []string, props map[string]string, stdin, stdout, stderr *os.File) (host.Handle, error) {
	f.calls = append(f.calls, "start")
	f.grant, f.argv, f.env, f.props = g, argv, env, props
	if f.startEr != nil {
		return host.Handle{}, f.startEr
	}
	f.stdin, _ = io.ReadAll(stdin)
	if _, err := stdout.WriteString(f.report); err != nil {
		return host.Handle{}, err
	}
	return host.Handle{Run: g.Run, Token: g.Token, Unit: "fab-sweep.service"}, nil
}

func (f *fakeSweepManager) Wait(h host.Handle) (int, error) {
	f.calls = append(f.calls, "wait")
	return 0, nil
}

func (f *fakeSweepManager) Stop(h host.Handle, reason string, grace time.Duration) (host.StopReceipt, error) {
	f.calls = append(f.calls, "stop")
	return f.stopRC, nil
}

type sweepFixture struct {
	streams, attempts, runDir string
	opt                       m1SweepOptions
	mgr                       *fakeSweepManager
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	root := t.TempDir()
	f := &sweepFixture{streams: filepath.Join(root, "streams"), attempts: filepath.Join(root, "attempts"), runDir: filepath.Join(root, "run", "r_sw_3")}
	for _, d := range []string{filepath.Join(f.streams, "r_sw_3"), filepath.Join(f.attempts, "r_sw_3", "wt"), f.runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.mgr = &fakeSweepManager{report: `{"complete":true}`, stopRC: host.StopReceipt{Verified: true}}
	f.opt = m1SweepOptions{mgr: f.mgr, streams: f.streams, attemptRoot: f.attempts,
		base: host.Grant{Model: "m", Effort: "high", Rung: "R1", TierTableSHA: "t", Profile: "P0", MaxTurns: 40, MaxBudgetUSD: 2, Wall: time.Minute, ConfigSHA: "n/a"},
		fence: m1SweepFence(func(m domain.MissionID) (domain.Mission, error) {
			return domain.Mission{ID: m, Attempts: map[domain.AttemptID]domain.Attempt{"r_sw_3": {ID: "r_sw_3", Fence: 3}}}, nil
		})}
	return f
}

// goodqa#15966 AC-35(h): the factory rebuilds the attempt's grant from its
// run record and runs the sweep through StartIOWithProperties with exactly
// the sweep unit's limits, then waits and stops it; stdin reaches the unit
// and the report comes back; no stdin or stdout file is left behind.
func TestM1SweepForStartsThroughManagerWithProperties(t *testing.T) {
	f := newSweepFixture(t)
	launch, err := m1SweepFor(f.opt)("m_sw", "r_sw_3", f.runDir)
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"/srv/fabric/bin/fab", "__sweep", "--", filepath.Join(f.runDir, "home")}
	out, code, err := launch(argv, sweep.Env, []byte(`{"input":1}`))
	if err != nil || code != 0 || string(out) != f.mgr.report {
		t.Fatalf("launch: out=%q code=%d err=%v", out, code, err)
	}
	want := map[string]string{"RuntimeMaxSec": "125", "MemoryMax": "256M", "LimitCORE": "0"}
	if !reflect.DeepEqual(f.mgr.props, want) {
		t.Fatalf("unit properties %v, want %v", f.mgr.props, want)
	}
	if _, err := host.UnitPropertyArgs(f.mgr.props); err != nil {
		t.Fatalf("the sweep's own properties are not allowlisted: %v", err)
	}
	g := f.mgr.grant
	if g.Run != "r_sw_3" || g.Mission != "m_sw" || g.Fence != 3 || g.RunDir != f.runDir || g.Worktree != filepath.Join(f.attempts, "r_sw_3", "wt") || g.Profile != "P0" {
		t.Fatalf("grant not rebuilt from the run record: %+v", g)
	}
	if !strings.HasPrefix(g.Token, "sweep-") || g.Validate() != nil {
		t.Fatalf("sweep grant token %q or validation %v", g.Token, g.Validate())
	}
	if !reflect.DeepEqual(f.mgr.calls, []string{"start", "wait", "stop"}) || !reflect.DeepEqual(f.mgr.argv, argv) || !reflect.DeepEqual(f.mgr.env, sweep.Env) {
		t.Fatalf("calls %v argv %v env %v", f.mgr.calls, f.mgr.argv, f.mgr.env)
	}
	if string(f.mgr.stdin) != `{"input":1}` {
		t.Fatalf("stdin %q", f.mgr.stdin)
	}
	left, _ := os.ReadDir(filepath.Join(f.streams, "r_sw_3"))
	if len(left) != 0 {
		t.Fatalf("sweep files left in the stream directory: %v", left)
	}
}

// A missing run record, an unverified stop of the sweep unit, or a start
// error each fail the launch, so the settle reads sweep_incomplete.
func TestM1SweepForFailures(t *testing.T) {
	f := newSweepFixture(t)
	if _, err := m1SweepFor(f.opt)("m_sw", "r_sw_9", f.runDir); err == nil {
		t.Fatal("an attempt without a run record got a launcher")
	}
	f.mgr.stopRC = host.StopReceipt{Verified: false}
	launch, err := m1SweepFor(f.opt)("m_sw", "r_sw_3", f.runDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := launch([]string{"fab", "__sweep", "--"}, sweep.Env, nil); err == nil {
		t.Fatal("an unverified sweep-unit stop returned a report")
	}
	f.mgr.stopRC, f.mgr.startEr = host.StopReceipt{Verified: true}, errors.New("busy")
	if _, _, err := launch([]string{"fab", "__sweep", "--"}, sweep.Env, nil); err == nil {
		t.Fatal("a start error returned a report")
	}
}
