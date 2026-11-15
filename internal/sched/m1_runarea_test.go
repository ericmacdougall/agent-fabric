package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
	"fabric/internal/storetest"
)

// goodqa#15966 AC-15, AC-18, AC-29 (owner ruling A): no worker variable,
// argument or grant path names the fabd-only run area, which holds the
// streams, the binding records and the AC-29 records.
func TestRunAreaNeverReachesTheWorker(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	runArea := filepath.Join(root, "run-area")
	for _, d := range []string{secret, runArea, filepath.Join(runArea, "bindings"), filepath.Join(runArea, "streams"), filepath.Join(runArea, "records")} {
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
	admitSpec(t, st, "m_ra", domain.ProviderClaudeCode, 1)
	ln := &fakeLauncher{st: st, block: make(chan struct{})}
	t.Cleanup(func() { close(ln.block) })
	work := t.TempDir()
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: work, RunnerDSN: storetest.RunnerDSN(t, st),
		StreamRoot: filepath.Join(runArea, "streams"), RecordsDir: filepath.Join(runArea, "records"),
		Credential: &CredentialLaunch{Policy: pol, BindingsDir: filepath.Join(runArea, "bindings"), IDKey: make([]byte, 32), PinnedSHA: pin}}
	cfg.Launch.Binary = bin
	s := New(st, cfg, ln)
	if err := s.runOnce(ctx, "m_ra"); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, ln, 1)
	ln.mu.Lock()
	argv, env := ln.argvs[0], ln.envs[0]
	ln.mu.Unlock()
	for _, a := range argv {
		if strings.Contains(a, runArea) {
			t.Fatalf("worker argument names the run area: %q", a)
		}
	}
	for _, e := range env {
		if strings.Contains(e, runArea) {
			n, _, _ := strings.Cut(e, "=")
			t.Fatalf("worker variable %s names the run area", n)
		}
	}
	if len(env) == 0 || len(argv) == 0 {
		t.Fatal("no launch recorded: the check above is vacuous")
	}
}
