package main

import (
	"fmt"
	"os"
	"os/exec"

	"fabric/internal/dispatch"
	"fabric/internal/host"
)

// realProviderRefused is the goodqa#15976 AC-2 refusal: a real provider on a
// host profile that has not shown a worker boundary. It exits 3 (a refusal;
// nothing was started).
type realProviderRefused struct{ reason string }

func (e realProviderRefused) Error() string {
	return "refusing to run a real provider (goodqa#15976): " + e.reason +
		"; only a pinned fake (the stdin fake or the recorded fakeclaude, by sha256) may run here"
}

// providerIsPinnedFake hashes the resolved provider file. A file that cannot
// be hashed is real: identity comes from content, never from a name or a
// flag such as --trust-provider.
func providerIsPinnedFake(path string) bool {
	sha, err := dispatch.FileSHA256(path)
	return err == nil && dispatch.PinnedFake(sha)
}

// daemonProviderGate decides whether fabd may start with this provider
// (goodqa#15976 AC-2, AC-3). A provider is real unless its resolved file
// hashes to a pinned fake. --trust-provider is honoured only for a pinned
// fake and is refused otherwise, even with --no-dispatch (it would skip the
// negative controls). --no-dispatch starts nothing and is otherwise never
// refused. A real provider needs a profile that passes realProviderGate.
// fakeOnly reports that the start rests on the provider being a pinned
// fake: every launch must then re-check the file it resolves, because an
// update or a repointed symlink could swap in a real binary after the start.
func daemonProviderGate(noDispatch, trustProvider bool, providerPath string, p host.Profile) (fakeOnly bool, err error) {
	fake := providerIsPinnedFake(providerPath)
	if trustProvider && !fake {
		return false, realProviderRefused{fmt.Sprintf("--trust-provider is honoured only for a pinned fake, and %s does not hash to one", providerPath)}
	}
	if noDispatch || trustProvider {
		return trustProvider, nil
	}
	gateErr := realProviderGate(p)
	if gateErr == nil {
		return false, nil
	}
	if fake {
		return true, nil
	}
	return false, gateErr
}

// gateDaemonProvider applies daemonProviderGate to fabd's provider and, for
// a fake-only start, makes its launch re-check the resolved file at every
// launch (AC-3), so a binary swapped in after the start is refused.
func gateDaemonProvider(dp *daemonProvider, noDispatch, trustProvider bool, p host.Profile) error {
	fakeOnly, err := daemonProviderGate(noDispatch, trustProvider, dp.Path, p)
	if err != nil {
		return err
	}
	if fakeOnly {
		dp.Launch.Resolve = pinnedFakeResolve(dp.Launch.Resolve)
	}
	return nil
}

// nativeRunnerGate (goodqa#15976 AC-2): the native anthropic-api runner
// calls a real model, and M1 builds keep it off on every host. Only the
// existing test-only --unconfined-fake-host mode runs.
func nativeRunnerGate() error {
	return realProviderRefused{"the native anthropic-api runner is off in M1 builds (only --unconfined-fake-host, a test mode, runs)"}
}

// harnessProviderGate (goodqa#15976 AC-2) covers `fab harness run`: the
// claude-code adapter runs its binary unconfined, so only a pinned fake
// runs; a codex model turn (--codex-run) is a real provider and is refused;
// the codex preflight runs no model turn; native is nativeRunnerGate.
func harnessProviderGate(adapter, claudeBin string, codexRun, unconfinedFake bool) error {
	switch adapter {
	case "native":
		if !unconfinedFake {
			return nativeRunnerGate()
		}
	case "claude-code":
		path, err := exec.LookPath(claudeBin)
		if err != nil || !(providerIsPinnedFake(path) || harnessTestFake(path)) {
			return realProviderRefused{fmt.Sprintf("the claude-code harness adapter runs unconfined, and %q is not a pinned fake", claudeBin)}
		}
	case "codex":
		if codexRun {
			return realProviderRefused{"a codex model turn (--codex-run) is a real provider"}
		}
	}
	return nil
}

// demoProviderGate (goodqa#15976 AC-2): `fab harness demo` always drives a
// real native client, so M1 builds refuse it.
func demoProviderGate(provider string) error {
	return realProviderRefused{fmt.Sprintf("fab harness demo runs the real %s client", provider)}
}

// harnessTestPins holds the sha256 of the throwaway claude fakes the
// harness tests write (their behaviour is the subject of those tests). Only
// test code in this package sets it; it is nil in a built binary.
var harnessTestPins map[string]bool

func harnessTestFake(path string) bool {
	if harnessTestPins == nil {
		return false
	}
	sha, err := dispatch.FileSHA256(path)
	return err == nil && harnessTestPins[sha]
}

// pinnedFakeResolve wraps a launch's Resolve so each launch runs only a
// pinned fake: any other resolved file refuses that launch ("" is Resolve's
// refusal; claude.Argv never falls back to the name).
func pinnedFakeResolve(resolve func(string) string) func(string) string {
	return func(name string) string {
		p := resolve(name)
		if p == "" {
			return ""
		}
		if !providerIsPinnedFake(p) {
			fmt.Fprintf(os.Stderr, "fab: refusing launch (goodqa#15976): provider %s is not a pinned fake and this host has not shown a worker boundary\n", p)
			return ""
		}
		return p
	}
}
