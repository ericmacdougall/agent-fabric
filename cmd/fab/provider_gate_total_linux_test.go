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

// goodqa#15976 PR #57 v3 F1: the daemon gate is total. Every combination
// of --no-dispatch, --trust-provider, provider kind and profile ends in
// exactly one of: (a) a pinned fake whose launches resolve only to its
// sealed copy, (b) a real provider behind a verified worker boundary, or
// (c) an exit-3 refusal. Never a start whose resolver returns an
// unverified file. Only symbols that predate the fix, so this runs
// unchanged against 183462f8.
func TestDaemonGateIsTotal(t *testing.T) {
	dir := t.TempDir()
	stdinFake := filepath.Join(dir, "fakeclaude-stdin")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"), stdinFake)
	recorded := filepath.Join(dir, "fakeclaude-live")
	copyFile(t, filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude.sh"), recorded)
	realClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	supported := true
	orig := managerSupport
	managerSupport = func(*host.LinuxManager) error {
		if supported {
			return nil
		}
		return errors.New("linux manager unsupported: PrivatePIDs needs systemd>=257 (found 255)")
	}
	t.Cleanup(func() { managerSupport = orig })
	profiles := []struct {
		name      string
		make      func(user string) host.Profile
		supported bool
		manager   bool
	}{
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
		{"real", realClaude, false},
		{"unhashable", filepath.Join(dir, "gone"), false},
	}
	for _, pc := range profiles {
		for _, user := range []string{"", "fab-worker"} {
			for _, pv := range providers {
				for _, noDispatch := range []bool{false, true} {
					for _, trust := range []bool{false, true} {
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
							schema := filepath.Join(t.TempDir(), "checkpoint.schema.json")
							dp := daemonProvider{Path: pv.path, Launch: productionLaunch(pv.path, schema, ""), SealRoot: sealRootFor(schema)}
							real, err := gateDaemonProvider(&dp, noDispatch, trust, pc.make(user))
							switch {
							case err != nil: // (c)
								wantRealProviderRefusal(t, err, name)
								if pv.fake {
									t.Errorf("a pinned fake was refused: %v", err)
								}
							case real: // (b)
								if pv.fake || !eligible {
									t.Fatalf("outcome real for fake=%v eligible=%v; a real start needs a non-fake provider behind a verified worker boundary", pv.fake, eligible)
								}
							default: // (a)
								got := dp.Launch.Resolve(dp.Launch.Binary)
								root, rerr := filepath.EvalSymlinks(dp.SealRoot)
								if !pv.fake || rerr != nil || !strings.HasPrefix(got, root+string(filepath.Separator)) {
									t.Fatalf("a non-real start resolves to %q (sealed root %s, %v); want the sealed copy of a pinned fake", got, dp.SealRoot, rerr)
								}
							}
						})
					}
				}
			}
		}
	}
}

// The exact receipt case through fab daemon: --no-dispatch with an
// unpinned provider and no worker boundary is refused with exit 3 before
// the work root exists (it used to start, and the startup controls ran the
// unverified file).
func TestDaemonNoDispatchRefusesAnUnpinnedProvider(t *testing.T) {
	t.Setenv("FABRIC_LINUX_WORKER_MANAGER", "")
	t.Setenv("FABRIC_WORKER_USER", "")
	t.Setenv("FABRIC_RUNNER_DSN_ENV", "")
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
	for _, user := range []string{"", "fab-worker"} {
		wr := filepath.Join(t.TempDir(), "wr")
		err := daemonMain([]string{"--work-root", wr, "--provider", realClaude, "--tier-table", tiers, "--rung", "r1", "--once", "--no-dispatch", "--worker-user", user})
		wantRealProviderRefusal(t, err, "no-dispatch, unpinned provider, worker-user="+user)
		if _, statErr := os.Stat(wr); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("worker-user=%s: the work root was created before the refusal (%v)", user, statErr)
		}
	}
}
