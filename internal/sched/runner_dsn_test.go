package sched

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

// goodqa#15173 (Kimi K1): a worker never receives the scheduler's own
// database login. Without a RunnerDSN nothing launches; with one, the
// runner role must pass CheckRunnerRole before the first launch, gate or no
// gate, whether the launch comes through Tick or a direct runOnce.
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

	// An empty or blank RunnerDSN refuses every launch, gated or not, through
	// Tick and through a direct runOnce: the worker never starts with the
	// scheduler's FABRIC_DSN or with no database login at all.
	for _, dsn := range []string{"", "  "} {
		t.Run("ungated without a runner login "+strconv.Quote(dsn)+": refused before any launch", func(t *testing.T) {
			t.Setenv("FABRIC_DSN", canary)
			s, ln := newSched(t, dsn)
			if _, err := s.Tick(ctx); err == nil || !strings.Contains(err.Error(), "RunnerDSN") {
				t.Fatalf("Tick without a runner login: %v, want a RunnerDSN refusal", err)
			}
			if err := s.runOnce(ctx, "m_k1"); err == nil || !strings.Contains(err.Error(), "RunnerDSN") {
				t.Fatalf("runOnce without a runner login: %v, want a RunnerDSN refusal", err)
			}
			if n := atomic.LoadInt32(&ln.starts); n != 0 {
				t.Fatalf("%d launches without a runner login", n)
			}
		})
	}

	t.Run("ungated with a checked runner login: the worker gets exactly it", func(t *testing.T) {
		t.Setenv("FABRIC_DSN_DATABASE", "unapproved_parent_target")
		t.Setenv("FABRIC_RUNNER_DSN_DATABASE", "unapproved_parent_target")
		t.Setenv("FABRIC_REVIEW_DSN_PASSWORD_FILE", "discard_parent_source")
		t.Setenv("FABRIC_DSN", canary)
		st := storetest.Open(t)
		setCap(t, st, 5)
		admitSpec(t, st, "m_k1", domain.ProviderClaudeCode, 1)
		runner := storetest.RunnerDSN(t, st)
		ln := &fakeLauncher{st: st, block: make(chan struct{})}
		t.Cleanup(func() { close(ln.block) })
		cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
			MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: t.TempDir(), RunnerDSN: runner}
		cfg.Launch.Binary = "/bin/true"
		s := New(st, cfg, ln)
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
		var got []string
		for _, e := range env {
			if strings.Contains(e, "unapproved_parent_target") || strings.Contains(e, "discard_parent_source") {
				t.Fatal("worker inherited parent source metadata or target")
			}
			if strings.Contains(e, "K1-CANARY") {
				t.Fatal("the worker environment carries the scheduler's FABRIC_DSN")
			}
			if name, val, _ := strings.Cut(e, "="); name == "FABRIC_DSN" {
				got = append(got, val)
			}
		}
		if len(got) != 1 || got[0] != runner {
			t.Fatalf("worker got %d FABRIC_DSN entries, want exactly the runner login", len(got))
		}
	})

	t.Run("direct runOnce with an owner-role runner login: refused before any launch", func(t *testing.T) {
		s, ln := newSched(t, storetest.DSN)
		if err := s.runOnce(ctx, "m_k1"); err == nil || !strings.Contains(err.Error(), "runner role") {
			t.Fatalf("runOnce with an owner-role runner login: %v, want a runner-role refusal", err)
		}
		if n := atomic.LoadInt32(&ln.starts); n != 0 {
			t.Fatalf("%d launches with an unchecked runner login", n)
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
