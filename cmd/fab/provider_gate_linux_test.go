//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/host"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// goodqa#15976 AC-2, NEG-2, NEG-3: profile x worker user x provider sha. A
// real provider (any file not hashing to a pinned fake, whatever its name)
// starts only under a supported LinuxManager with a worker account; the
// pinned fakes start anywhere, but only as fakes (every launch re-checks);
// --no-dispatch starts nothing and is never refused.
func TestDaemonProviderGateTable(t *testing.T) {
	dir := t.TempDir()
	stdinFake := filepath.Join(dir, "fakeclaude-stdin")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), stdinFake)
	recorded := filepath.Join(dir, "fakeclaude-live")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"), recorded)
	// NEG-3: named like the fake, different content.
	impostorDir := filepath.Join(dir, "impostor")
	if err := os.MkdirAll(impostorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	impostor := filepath.Join(impostorDir, "fakeclaude-stdin")
	if err := os.WriteFile(impostor, []byte("#!/bin/sh\nexec claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	realClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone")

	supported := true
	orig := managerSupport
	managerSupport = func(*host.LinuxManager) error {
		if supported {
			return nil
		}
		return errors.New("linux manager unsupported: PrivatePIDs needs systemd>=257 (found 255)")
	}
	t.Cleanup(func() { managerSupport = orig })

	type profileCase struct {
		name      string
		make      func(user string) host.Profile
		supported bool
		manager   bool
	}
	profiles := []profileCase{
		{"legacy", func(u string) host.Profile { return &host.Linux{User: u} }, true, false},
		{"manager", func(u string) host.Profile { return &host.LinuxManager{User: u} }, true, true},
		{"manager-unsupported", func(u string) host.Profile { return &host.LinuxManager{User: u} }, false, true},
	}
	providers := []struct {
		name, path string
		fake       bool
	}{
		{"stdin-fake", stdinFake, true},
		{"recorded-fake", recorded, true},
		{"impostor-named-fake", impostor, false},
		{"real", realClaude, false},
		{"unhashable", missing, false},
	}
	for _, pc := range profiles {
		for _, user := range []string{"", "fab-worker"} {
			for _, pv := range providers {
				for _, mode := range []struct{ noDispatch, trust bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
					noDispatch, trust := mode.noDispatch, mode.trust
					name := pc.name + "/user=" + user + "/" + pv.name
					if noDispatch {
						name += "/no-dispatch"
					}
					if trust {
						name += "/trust-provider"
					}
					t.Run(name, func(t *testing.T) {
						supported = pc.supported
						eligible := pc.manager && user != "" && pc.supported
						fakeOnly, err := daemonProviderGate(noDispatch, trust, pv.path, pc.make(user))
						switch {
						case trust && !pv.fake:
							// AC-3 / NEG-3: the flag is honoured only for a pinned fake.
							wantRealProviderRefusal(t, err, name)
							if !strings.Contains(err.Error(), "--trust-provider") {
								t.Errorf("refusal does not name --trust-provider: %v", err)
							}
						case trust:
							if err != nil || !fakeOnly {
								t.Fatalf("a trusted pinned fake must start fake-only (re-checked per launch), got fakeOnly=%v err=%v", fakeOnly, err)
							}
						case noDispatch, eligible:
							if err != nil || fakeOnly {
								t.Fatalf("want an unrestricted start, got fakeOnly=%v err=%v", fakeOnly, err)
							}
						case pv.fake:
							if err != nil || !fakeOnly {
								t.Fatalf("a pinned fake must start fake-only, got fakeOnly=%v err=%v", fakeOnly, err)
							}
						default:
							wantRealProviderRefusal(t, err, name)
							msg := err.Error()
							if !pc.manager && !strings.Contains(msg, "legacy Linux profile") {
								t.Errorf("refusal does not name the legacy profile: %s", msg)
							}
							if user == "" && !strings.Contains(msg, "FABRIC_WORKER_USER is empty") {
								t.Errorf("refusal does not name the empty worker user: %s", msg)
							}
							if pc.manager && user != "" && !pc.supported && !strings.Contains(msg, "unsupported") {
								t.Errorf("refusal does not name the unsupported manager: %s", msg)
							}
						}
					})
				}
			}
		}
	}
}

// A start allowed only for a pinned fake re-checks the file at every
// launch: a binary swapped in after the start is refused.
func TestPinnedFakeResolveRechecksEachLaunch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fakeclaude-stdin")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), p)
	resolve := pinnedFakeResolve(func(string) string { return p })
	if got := resolve("fakeclaude-stdin"); got != p {
		t.Fatalf("pinned fake refused: %q", got)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolve("fakeclaude-stdin"); got != "" {
		t.Fatalf("a swapped-in real binary resolved: %q", got)
	}
	if got := pinnedFakeResolve(func(string) string { return "" })("x"); got != "" {
		t.Fatalf("an unresolved name resolved: %q", got)
	}
}

// goodqa#15976 AC-2 / NEG-4: `fab harness run` refuses every real provider
// before it writes the event file or starts a process: the native runner
// (except its test-only fake host), the unconfined claude-code adapter with
// anything but a pinned fake, and a codex model turn.
func TestHarnessProviderGate(t *testing.T) {
	dir := t.TempDir()
	stdinFake := filepath.Join(dir, "fakeclaude-stdin")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), stdinFake)
	realClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, adapter, bin   string
		codexRun, fake, want bool
	}{
		{"native real", "native", "claude", false, false, false},
		{"native unconfined fake host", "native", "claude", false, true, true},
		{"claude-code real", "claude-code", realClaude, false, false, false},
		{"claude-code missing", "claude-code", filepath.Join(dir, "none"), false, false, false},
		{"claude-code pinned fake", "claude-code", stdinFake, false, false, true},
		{"codex run", "codex", "claude", true, false, false},
		{"codex preflight", "codex", "claude", false, false, true},
	} {
		err := harnessProviderGate(c.adapter, c.bin, c.codexRun, c.fake)
		if c.want {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
			continue
		}
		wantRealProviderRefusal(t, err, c.name)
	}
	// Through the command: the refusal comes before the event file exists.
	ev := filepath.Join(dir, "events.jsonl")
	err := harnessMain([]string{"run", "--events", ev, "--adapter", "claude-code", "--unconfined", "--claude-bin", realClaude, "--prompt", "hi", "--wait", "1s"})
	wantRealProviderRefusal(t, err, "harness run claude-code real")
	if _, statErr := os.Stat(ev); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the event file was created before the refusal (%v)", statErr)
	}
}
