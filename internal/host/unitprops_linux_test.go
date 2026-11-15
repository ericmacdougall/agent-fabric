//go:build linux

package host

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/egress"
)

// goodqa#15966 AC-35(h): a property outside the allowlist, or a malformed
// value, is refused before anything runs: no manager call (not even the
// worker uid lookup), no preflight unit, and the run gate stays free.
func TestStartIOWithPropertiesRefusesUnknownKey(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown key":          {"RuntimeMaxSec": "125", "ExecStartPre": "/bin/sh"},
		"malformed MemoryMax":  {"MemoryMax": "256M\n-p ExecStartPre=/bin/sh"},
		"LimitCORE raised":     {"LimitCORE": "infinity"},
		"RuntimeMaxSec zero":   {"RuntimeMaxSec": "0"},
		"RuntimeMaxSec spaces": {"RuntimeMaxSec": " 125"},
	}
	for name, props := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			l := &LinuxManager{Self: "/srv/fabric/bin/fab", User: "fab-worker", LockPath: filepath.Join(t.TempDir(), "locks"),
				commandFn: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					calls++
					return exec.CommandContext(ctx, "false")
				}}
			null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			_, err = l.StartIOWithProperties(controlGrant(t.TempDir()), []string{"/srv/fabric/bin/fab", "__sweep"}, nil, props, null, null, null)
			if calls != 0 {
				t.Fatalf("%d manager calls before the refusal; want 0 (err %v)", calls, err)
			}
			if !errors.Is(err, ErrUnitProperty) {
				t.Fatalf("err = %v, want ErrUnitProperty", err)
			}
			if !l.runGate.tryLock() {
				t.Fatal("the refusal left the run gate held")
			}
			l.runGate.unlock()
		})
	}
}

// The sweep's limits reach the unit as systemd properties, each exactly once:
// they replace the manager default of the same name.
func TestUnitPropertiesReachUnitArgs(t *testing.T) {
	props, err := UnitPropertyArgs(map[string]string{"RuntimeMaxSec": "125", "MemoryMax": "256M", "LimitCORE": "0"})
	if err != nil {
		t.Fatal(err)
	}
	l := &LinuxManager{Self: "/srv/fabric/bin/fab", MemoryMax: "1G"}
	args, _, err := l.startArgsEnvWithPolicy(controlGrant(t.TempDir()), "fab-sweep.service", "/run/ef", []string{"/srv/fabric/bin/fab", "__sweep"}, nil, nil, props)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for i := 0; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] == "-p" {
			got[args[i+1]]++
			i++
		}
	}
	for _, want := range []string{"RuntimeMaxSec=125", "MemoryMax=256M", "LimitCORE=0"} {
		if got[want] != 1 {
			t.Errorf("unit property %s appears %d times before the command; want 1 (%v)", want, got[want], args)
		}
	}
	for p, n := range got {
		if strings.HasPrefix(p, "MemoryMax=") && p != "MemoryMax=256M" {
			t.Errorf("manager default %s (x%d) kept beside the sweep's MemoryMax", p, n)
		}
	}
}

// The sweep unit gets no egress mediator (owner ruling on B): its network
// plan is the policy-nil one (IPAddressDeny=any, loopback only, the plain
// manager preflight, never the egress preflight), and the worker's AC-20
// egress counts are byte-identical after it. Plain StartIO on the same
// manager is the control: it does attach the mediator.
func TestStartIOWithPropertiesHasNoMediator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mediated bool
	}{{"sweep", false}, {"worker control", true}} {
		t.Run(tc.name, func(t *testing.T) {
			counts := t.TempDir()
			g := controlGrant(t.TempDir())
			prior := []byte(`{"mission":"m_nc","attempt":"` + string(g.Run) + `","hosts":{"api.anthropic.com":{"bytes":42}}}`)
			countsFile := filepath.Join(counts, string(g.Run)+".egress.json")
			if err := os.WriteFile(countsFile, prior, 0o600); err != nil {
				t.Fatal(err)
			}
			var preflight []string
			locks := t.TempDir()
			if err := os.Chmod(locks, 0o700); err != nil {
				t.Fatal(err)
			}
			l := &LinuxManager{Self: "/srv/fabric/bin/fab", LockPath: filepath.Join(locks, "worker.lock"), EgressCountsDir: counts,
				EgressConfig: &egress.Config{Resolver: linuxTestResolver{ip: netip.MustParseAddr("93.184.216.34")}},
				commandFn: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					switch {
					case name == "systemctl" && len(args) == 1 && args[0] == "--version":
						return exec.CommandContext(ctx, "echo", "systemd 257 (257.9-1)")
					case name == "systemctl":
						return exec.CommandContext(ctx, "true") // no units loaded
					case name == "systemd-run" && preflight == nil:
						preflight = append([]string{}, args...)
					}
					return exec.CommandContext(ctx, "false") // stop at the preflight: nothing starts
				}}
			null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			if tc.mediated {
				_, err = l.StartIO(g, []string{"true"}, nil, null, null, null)
			} else {
				_, err = l.StartIOWithProperties(g, []string{"/srv/fabric/bin/fab", "__sweep"}, nil, map[string]string{"RuntimeMaxSec": "125"}, null, null, null)
			}
			if err == nil || preflight == nil {
				t.Fatalf("start err=%v preflight=%v; the fake should stop it at the preflight", err, preflight)
			}
			joined := strings.Join(preflight, " ")
			egressPreflight := strings.Contains(joined, "--egress-preflight")
			if egressPreflight != tc.mediated {
				t.Fatalf("egress preflight=%v, want %v: %s", egressPreflight, tc.mediated, joined)
			}
			if !tc.mediated && (!strings.Contains(joined, "--manager-preflight") || !strings.Contains(joined, "IPAddressDeny=any")) {
				t.Fatalf("sweep preflight is not the policy-nil plan: %s", joined)
			}
			if got, err := os.ReadFile(countsFile); err != nil || string(got) != string(prior) {
				t.Fatalf("worker egress counts changed: %q (%v)", got, err)
			}
		})
	}
}
