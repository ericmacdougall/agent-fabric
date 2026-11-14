//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
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
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"), filepath.Join(filepath.Dir(p), "fakeclaude-live"))
	resolve := pinnedFakeResolve(func(string) string { return p }, t.TempDir())
	if got := resolve("fakeclaude-stdin"); got == "" || got == p {
		t.Fatalf("pinned fake not launched from a sealed copy: %q", got)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolve("fakeclaude-stdin"); got != "" {
		t.Fatalf("a swapped-in real binary resolved: %q", got)
	}
	if got := pinnedFakeResolve(func(string) string { return "" }, t.TempDir())("x"); got != "" {
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

// goodqa#15976 AC-3: through fabd's own launch. A trusted start with the
// pinned fake launches it; after the file is swapped for a real binary the
// next launch is refused before any argv exists. Without the per-launch
// wrap the swapped binary would launch.
func TestTrustedLaunchRefusesASwappedBinary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fakeclaude-live")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"), p)
	dp, err := newDaemonProvider(p, filepath.Join(t.TempDir(), "checkpoint.schema.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if real, err := gateDaemonProvider(&dp, false, true, &host.Linux{User: "fab-worker"}); err != nil || real {
		t.Fatalf("a trusted pinned fake: real=%v err=%v", real, err)
	}
	l := dp.Launch
	l.Prompt = "checkpoint"
	g := launchGrant(t)
	if argv, _, err := claude.Argv(g, l); err != nil || !strings.HasPrefix(argv[0], dp.SealRoot+"/") {
		t.Fatalf("first launch argv=%v err=%v; want a sealed copy under %s", argv, err, dp.SealRoot)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if argv, _, err := claude.Argv(g, l); err == nil {
		t.Fatalf("the swapped binary launched: %v", argv)
	}
}

// With FABRIC_LINUX_WORKER_MANAGER=1, fabd builds its launcher from the
// worker manager instead of panicking on a *host.Linux assertion: the pinned
// stdin fake in registry mode gets past the gate and the launcher to the
// (closed) store.
func TestDaemonLaunchesUnderTheWorkerManager(t *testing.T) {
	t.Setenv("FABRIC_LINUX_WORKER_MANAGER", "1")
	t.Setenv("FABRIC_WORKER_USER", "")
	t.Setenv("FABRIC_RUNNER_DSN_ENV", "")
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
	err = daemonMain([]string{"--work-root", filepath.Join(t.TempDir(), "wr"), "--provider", pinned, "--tier-table", tiers, "--rung", "r1", "--once",
		"--dispatch-via=registry", "--trust-provider", "--worker-user", "fab-worker"})
	if err == nil || !strings.Contains(err.Error(), "failed to connect") {
		t.Fatalf("want the store connection error after the gate and the launcher, got %v", err)
	}
}

// goodqa#15976 AC-2: `fab harness demo` drives a real client and is refused
// before its workspace check or event file.
func TestHarnessDemoRefused(t *testing.T) {
	ev := filepath.Join(t.TempDir(), "events.jsonl")
	err := harnessDemoMain([]string{"--provider", "claude", "--workspace", filepath.Join(t.TempDir(), "ws"), "--events", ev})
	wantRealProviderRefusal(t, err, "harness demo")
	if _, statErr := os.Stat(ev); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the event file was created (%v)", statErr)
	}
}

// goodqa#15976 AC-5: verified exit cleanup is on by default when fabd may
// dispatch a real provider, and stays opt-in for the pinned fakes.
func TestExitCleanupDefaultsOnForRealProviders(t *testing.T) {
	t.Setenv("FABRIC_WORKER_EXIT_CLEANUP", "")
	orig := managerSupport
	managerSupport = func(*host.LinuxManager) error { return nil }
	t.Cleanup(func() { managerSupport = orig })
	dir := t.TempDir()
	realClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fakeclaude-stdin")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), fake)
	eligible := &host.LinuxManager{User: "fab-worker"}
	for _, c := range []struct {
		name, path string
		noDispatch bool
		wantReal   bool
	}{
		{"real provider under the manager", realClaude, false, true},
		{"pinned fake under the manager", fake, false, true},
		{"real provider, no-dispatch", realClaude, true, false},
	} {
		dp := daemonProvider{Path: c.path, Launch: productionLaunch(c.path, "/x", "")}
		real, err := gateDaemonProvider(&dp, c.noDispatch, false, eligible)
		if err != nil || real != c.wantReal {
			t.Fatalf("%s: real=%v err=%v, want real=%v", c.name, real, err, c.wantReal)
		}
		if got := daemonExitCleanup(real); got != c.wantReal {
			t.Errorf("%s: exit cleanup=%v, want %v", c.name, got, c.wantReal)
		}
	}
	legacyFake := daemonProvider{Path: fake, Launch: productionLaunch(fake, "/x", "")}
	real, err := gateDaemonProvider(&legacyFake, false, false, &host.Linux{User: "fab-worker"})
	if err != nil || real || daemonExitCleanup(real) {
		t.Errorf("a fake-only start: real=%v err=%v cleanup=%v; want cleanup opt-in", real, err, daemonExitCleanup(real))
	}
	t.Setenv("FABRIC_WORKER_EXIT_CLEANUP", "1")
	if !daemonExitCleanup(false) {
		t.Error("FABRIC_WORKER_EXIT_CLEANUP=1 no longer turns cleanup on")
	}
}

// The sealed copies live where only fabd can write: a group- or
// world-writable directory, or a symlink in its place, is refused; the copy
// is 0555 and a delegate with the wrong sha is never sealed.
func TestSealPinnedFakeRefusesUnsafeLayouts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "seal")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ownedSealDir(root); err == nil {
		t.Error("a world-writable seal directory was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := ownedSealDir(link); err == nil {
		t.Error("a symlinked seal directory was accepted")
	}
	bin := t.TempDir()
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), filepath.Join(bin, "fakeclaude-stdin"))
	if err := os.WriteFile(filepath.Join(bin, "fakeclaude-live"), []byte("#!/bin/sh\necho not the recorded fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "seal")
	if _, err := sealPinnedFake(filepath.Join(bin, "fakeclaude-stdin"), good); err == nil {
		t.Error("a stdin fake with an unpinned delegate was sealed")
	}
	// A delegate that is a pinned fake, but not the recorded one, is refused.
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), filepath.Join(bin, "fakeclaude-live"))
	if _, err := sealPinnedFake(filepath.Join(bin, "fakeclaude-stdin"), good); err == nil {
		t.Error("a stdin fake delegating to another stdin fake was sealed")
	}
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"), filepath.Join(bin, "fakeclaude-live"))
	sealed, err := sealPinnedFake(filepath.Join(bin, "fakeclaude-stdin"), good)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sealed, filepath.Join(filepath.Dir(sealed), "fakeclaude-live")} {
		st, err := os.Lstat(f)
		if err != nil || st.Mode().Perm() != 0o555 || !st.Mode().IsRegular() {
			t.Errorf("sealed %s: %v %v; want a regular 0555 file", f, st.Mode(), err)
		}
	}
}
