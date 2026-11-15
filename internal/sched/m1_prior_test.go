package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// tokenRig is a launchable token-path scheduler config (as in
// TestRunAreaNeverReachesTheWorker) for mission m.
func tokenRig(t *testing.T, m domain.MissionID) (*store.Store, *fakeLauncher, Config, string) {
	t.Helper()
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	runArea := filepath.Join(root, "run-area")
	bindings := filepath.Join(runArea, "bindings")
	for _, d := range []string{secret, runArea, bindings, filepath.Join(runArea, "streams"), filepath.Join(runArea, "records")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pol := credential.Policy{Dir: secret, WorkerUID: 999999, WorkerGIDs: []int{999998}, OwnerUID: -1, GroupGID: -1}
	if _, err := pol.Capture(synthTok(108)); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pin, _ := credential.FileSHA256(bin)
	st := storetest.Open(t)
	setCap(t, st, 5)
	admitSpec(t, st, m, domain.ProviderClaudeCode, 1)
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	work := t.TempDir()
	t.Cleanup(func() {
		close(ln.block)
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			var live int
			st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM attempts WHERE mission_id = $1 AND state IN ('LEASED','PREPARING','RUNNING')`, string(m)).Scan(&live) //nolint:errcheck
			if live == 0 {
				time.Sleep(100 * time.Millisecond)
				return
			}
		}
	})
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: work, RunnerDSN: storetest.RunnerDSN(t, st),
		StreamRoot: filepath.Join(runArea, "streams"), RecordsDir: filepath.Join(runArea, "records"),
		Credential: &CredentialLaunch{Policy: pol, BindingsDir: bindings, IDKey: make([]byte, 32), PinnedSHA: pin}}
	cfg.Launch.Binary = bin
	return st, ln, cfg, bindings
}

// finishDirect ends attempt a abandoned without any settle, as a fabd that
// died after FINISH and before its settle marker would leave it.
func finishDirect(t *testing.T, st *store.Store, m domain.MissionID, a domain.AttemptID) {
	t.Helper()
	ctx := context.Background()
	var fence int64
	if err := st.Pool().QueryRow(ctx, `SELECT fence FROM attempts WHERE id = $1`, string(a)).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("finish-direct:"+string(a)), domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: a, Fence: fence,
		Outcome: "abandoned", Reason: "test: finished without settle"}); err != nil {
		t.Fatal(err)
	}
}

// Spec v16 prior_attempt_unsettled: on the token path, an earlier attempt
// with a binding record and no settled marker refuses the next START (no
// attempt, no event, 0 processes); once it is marked settled the next
// attempt starts.
func TestM1PriorAttemptUnsettledRefusesStart(t *testing.T) {
	ctx := context.Background()
	const m = domain.MissionID("m_pu")
	st, ln, cfg, bindings := tokenRig(t, m)
	s := New(st, cfg, ln)
	if err := s.runOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, ln, 1)
	const first = domain.AttemptID("r_pu_1")
	if _, err := os.Lstat(filepath.Join(bindings, string(first)+".json")); err != nil {
		t.Fatal("fixture: the first attempt has no binding record")
	}
	finishDirect(t, st, m, first)
	countEv := func() int {
		var n int
		st.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE mission_id = $1`, string(m)).Scan(&n) //nolint:errcheck
		return n
	}
	before := countEv()
	err := s.runOnce(ctx, m)
	if err == nil || !strings.Contains(err.Error(), "prior_attempt_unsettled") {
		t.Fatalf("runOnce: %v, want a prior_attempt_unsettled refusal", err)
	}
	if n := atomic.LoadInt32(&ln.starts); n != 1 {
		t.Fatalf("%d processes, want only the first", n)
	}
	if after := countEv(); after != before {
		t.Fatalf("events %d -> %d", before, after)
	}
	if err := credential.MarkSettled(bindings, string(first)); err != nil {
		t.Fatal(err)
	}
	if err := s.runOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, ln, 2)
}

// Spec v16 scope: an earlier attempt that never got a binding (abandoned
// before it, or a pre-M1 attempt) does not block START.
func TestM1PriorAttemptWithoutBindingDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	const m = domain.MissionID("m_pn")
	st, ln, cfg, _ := tokenRig(t, m)
	if _, err := st.Execute(ctx, "fabd", "start:r_pn_1", domain.Command{Kind: domain.CmdStartAttempt, Path: domain.ProviderClaudeCode, Mission: m, Attempt: "r_pn_1", Host: cfg.Host, Role: "writer"}); err != nil {
		t.Fatal(err)
	}
	finishDirect(t, st, m, "r_pn_1")
	s := New(st, cfg, ln)
	if err := s.runOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, ln, 1)
}

// bindingCheckingLauncher fails the test when a worker is launched with no
// durable binding record for its attempt.
type bindingCheckingLauncher struct {
	*fakeLauncher
	bindings string
	missing  atomic.Int32
}

func (b *bindingCheckingLauncher) Start(g host.Grant, argv, env []string, stdout, stderr *os.File) (host.Handle, error) {
	if _, err := os.Lstat(filepath.Join(b.bindings, string(g.Run)+".json")); err != nil {
		b.missing.Add(1)
	}
	return b.fakeLauncher.Start(g, argv, env, stdout, stderr)
}

// The invariant that makes the prior_attempt_unsettled scope safe: on the
// token path, no worker reaches launch without its attempt's binding
// record on disk, so an attempt with no record never ran with a token.
func TestM1NoLaunchWithoutBindingRecord(t *testing.T) {
	ctx := context.Background()
	const m = domain.MissionID("m_pb")
	st, ln, cfg, bindings := tokenRig(t, m)
	bl := &bindingCheckingLauncher{fakeLauncher: ln, bindings: bindings}
	s := New(st, cfg, bl)
	for i := 1; i <= 2; i++ {
		if err := s.runOnce(ctx, m); err != nil {
			t.Fatal(err)
		}
		waitStarts(t, ln, int32(i))
		a := domain.AttemptID("r_pb_" + string(rune('0'+i)))
		finishDirect(t, st, m, a)
		if err := credential.MarkSettled(bindings, string(a)); err != nil {
			t.Fatal(err)
		}
	}
	if n := bl.missing.Load(); n != 0 {
		t.Fatalf("%d launches without a binding record", n)
	}
}
