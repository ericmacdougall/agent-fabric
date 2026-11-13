package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15373 AC-2, NEG-11, NEG-12: fab daemon refuses a bad dispatch
// configuration with a named error before it opens the store, so a
// restarting unit never crash-loops on it; registry mode needs
// --trust-provider and the pinned stdin fake. The store DSN points at a
// closed port: a check that ran inside withStoreService would surface as a
// connection error instead of the named refusal.
func TestDaemonRegistryConfigFailsFast(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://fabd@127.0.0.1:1/none?connect_timeout=1")
	t.Setenv("FABRIC_RUNNER_DSN", "postgres://runner@127.0.0.1:1/none?connect_timeout=1")
	tiers := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(tiers, []byte(`{"rungs":{"r1":{"effort":"medium","models":{"claude":"m"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned, err := filepath.Abs(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other-provider")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(provider string, extra ...string) error {
		args := append([]string{"--work-root", filepath.Join(t.TempDir(), "wr"), "--provider", provider, "--tier-table", tiers, "--rung", "r1", "--once", "--worker-user", ""}, extra...)
		return daemonMain(args)
	}
	refusals := []struct {
		name     string
		provider string
		args     []string
		want     string
	}{
		{"registry without trust", pinned, []string{"--dispatch-via", "registry"}, "without --trust-provider"},
		{"registry with trust and a non-fake provider", other, []string{"--dispatch-via", "registry", "--trust-provider"}, "not the pinned stdin fake"},
		{"unknown dispatch-via", other, []string{"--dispatch-via", "regsitry", "--trust-provider"}, "unknown dispatch path"},
		{"negative launch limit", other, []string{"--launch-limit", "-1", "--trust-provider"}, "negative"},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			err := run(c.provider, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q before the store opens, got %v", c.want, err)
			}
		})
	}
	t.Run("registry with trust and the pinned fake passes the checks", func(t *testing.T) {
		err := run(pinned, "--dispatch-via", "registry", "--trust-provider")
		if err == nil {
			t.Fatal("the daemon ran against a closed port")
		}
		t.Logf("past the configuration checks: %v", err)
		if !strings.Contains(err.Error(), "failed to connect") {
			t.Fatalf("want the store connection error after the checks, got %v", err)
		}
		for _, cfgErr := range []string{"refusing --dispatch-via", "sched:", "pinned stdin fake"} {
			if strings.Contains(err.Error(), cfgErr) {
				t.Fatalf("a valid registry configuration was refused: %v", err)
			}
		}
	})
}
