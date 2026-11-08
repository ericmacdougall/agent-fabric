package sched

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

// goodqa#15173 (Kimi K1): a worker never receives the scheduler's own
// database login. Without a RunnerDSN the worker gets no FABRIC_DSN at all
// (its fab calls fail closed); with one, the runner role must pass
// CheckRunnerRole before the first launch, gate or no gate.
func TestWorkerNeverGetsSchedulerDSN(t *testing.T) {
	ctx := context.Background()
	const canary = "postgres://scheduler:K1-CANARY@db.invalid/fabric"
	newSched := func(t *testing.T, runnerDSN string) (*Scheduler, *fakeLauncher) {
		st := storetest.Open(t)
		setCap(t, st, 5)
		admitSpec(t, st, "m_k1", domain.ProviderClaudeCode, 1)
		ln := &fakeLauncher{st: st, block: make(chan struct{})}
		t.Cleanup(func() { close(ln.block) })
		cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
			MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir(), RunnerDSN: runnerDSN}
		cfg.Launch.Binary = "/bin/true"
		return New(st, cfg, ln), ln
	}

	t.Run("ungated without a runner login: no FABRIC_DSN reaches the worker", func(t *testing.T) {
		t.Setenv("FABRIC_DSN", canary)
		s, ln := newSched(t, "")
		if err := s.runOnce(ctx, "m_k1"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&ln.starts) < 1 {
			if time.Now().After(deadline) {
				t.Fatal("never launched")
			}
			time.Sleep(10 * time.Millisecond)
		}
		ln.mu.Lock()
		env := ln.envs[0]
		ln.mu.Unlock()
		for _, e := range env {
			if strings.Contains(e, "K1-CANARY") {
				t.Fatal("the worker environment carries the scheduler's FABRIC_DSN")
			}
			if name, val, _ := strings.Cut(e, "="); name == "FABRIC_DSN" && val != "" {
				t.Fatal("the worker got a FABRIC_DSN without a configured runner login")
			}
		}
	})

	t.Run("ungated with a runner login that is not a runner role: refused before any launch", func(t *testing.T) {
		// The test login owns the schema, so CheckRunnerRole refuses it.
		s, ln := newSched(t, storetest.DSN)
		_, err := s.Tick(ctx)
		if err == nil || !strings.Contains(err.Error(), "runner role") {
			t.Fatalf("Tick with an owner-role runner login: %v, want a runner-role refusal", err)
		}
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Fatalf("%d launches with an unchecked runner login", n)
		}
	})
}
