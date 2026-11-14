//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/native"
)

// wantRealProviderRefusal: the refusal names goodqa#15976 and exits 3.
func wantRealProviderRefusal(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "goodqa#15976") {
		t.Fatalf("%s: want a goodqa#15976 refusal, got %v", what, err)
	}
	if code := exitCode(err); code != 3 {
		t.Errorf("%s: exit %d, want 3", what, code)
	}
}

// goodqa#15976 AC-2 through fab daemon itself (only symbols that predate the
// gate, so this test runs unchanged against 96bc0ad and fails there): on the
// legacy profile a real provider is refused with exit 3 before the work root
// exists and before the store is opened, with or without a worker account,
// and --trust-provider does not lift it. The pinned stdin fake and
// --no-dispatch get past the gate to the (closed) store.
func TestDaemonRefusesRealProviderOnLegacyLinux(t *testing.T) {
	t.Setenv("FABRIC_LINUX_WORKER_MANAGER", "")
	t.Setenv("FABRIC_WORKER_USER", "")    // daemonMain sets it from --worker-user; restored after
	t.Setenv("FABRIC_RUNNER_DSN_ENV", "") // likewise
	t.Setenv("FABRIC_DSN", "postgres://fabd@127.0.0.1:1/none?connect_timeout=1")
	t.Setenv("FABRIC_RUNNER_DSN", "postgres://runner@127.0.0.1:1/none?connect_timeout=1")
	tiers := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(tiers, []byte(`{"rungs":{"r1":{"effort":"medium","models":{"claude":"m"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	realClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned, err := filepath.Abs(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := filepath.Abs(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(workRoot, provider string, extra ...string) error {
		args := append([]string{"--work-root", workRoot, "--provider", provider, "--tier-table", tiers, "--rung", "r1", "--once"}, extra...)
		return daemonMain(args)
	}
	for _, user := range []string{"", "fab-worker"} {
		t.Run("real provider, trust-provider, worker-user="+user, func(t *testing.T) {
			wr := filepath.Join(t.TempDir(), "wr")
			err := run(wr, realClaude, "--trust-provider", "--worker-user", user)
			wantRealProviderRefusal(t, err, "legacy profile")
			if _, statErr := os.Stat(wr); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the work root was created before the refusal (%v)", statErr)
			}
		})
	}
	// AC-3: --trust-provider with a real binary is refused even with
	// --no-dispatch (the flag skips the negative controls).
	t.Run("real provider, trust-provider, no-dispatch", func(t *testing.T) {
		wr := filepath.Join(t.TempDir(), "wr")
		wantRealProviderRefusal(t, run(wr, realClaude, "--no-dispatch", "--trust-provider", "--worker-user", ""), "trust with a real binary")
		if _, statErr := os.Stat(wr); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the work root was created before the refusal (%v)", statErr)
		}
	})
	for _, c := range []struct {
		name, provider string
		extra          []string
	}{
		{"stdin fake via registry (D1 d1-step)", pinned, []string{"--dispatch-via=registry", "--launch-limit", "1", "--trust-provider", "--worker-user", "fab-worker"}},
		{"no-dispatch with the recorded fake (the live unit)", recorded, []string{"--no-dispatch", "--trust-provider", "--worker-user", ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := run(filepath.Join(t.TempDir(), "wr"), c.provider, c.extra...)
			if err == nil || !strings.Contains(err.Error(), "failed to connect") || strings.Contains(err.Error(), "goodqa#15976") {
				t.Fatalf("want the store connection error after the gate, got %v", err)
			}
		})
	}
}

// goodqa#15976 AC-2: `fab mission run` (the native anthropic-api runner) is
// always a real provider, so on the legacy profile it is refused with exit 3
// before its run dir or any control. The run dir here cannot be created, so a
// build without the gate fails on it instead of starting a control.
func TestNativeOptionsRefusesOnLegacyLinux(t *testing.T) {
	t.Setenv("FABRIC_LINUX_WORKER_MANAGER", "")
	for _, user := range []string{"", "fab-worker"} {
		t.Setenv("FABRIC_WORKER_USER", user)
		_, err := nativeOptions(native.Grant{RunDir: "/dev/null/fab-15976"}, false)
		wantRealProviderRefusal(t, err, "native, worker-user="+user)
	}
}
