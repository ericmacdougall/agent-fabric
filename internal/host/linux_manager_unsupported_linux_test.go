//go:build linux

package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// fakeManagerCommands answers systemctl --version with version and fails
// systemd-run with preflight output, as observed on t2-dev (Ubuntu 24.04).
func fakeManagerCommands(t *testing.T, version, preflight string, ran *[]string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*ran = append(*ran, name)
		switch name {
		case "systemctl":
			return exec.CommandContext(ctx, "printf", "%s", version)
		case "systemd-run":
			return exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$1"; exit 1`, "sh", preflight)
		}
		t.Fatalf("unexpected command %s %v", name, args)
		return nil
	}
}

func captureManagerLog(t *testing.T) *[]string {
	var lines []string
	orig := managerLogf
	managerLogf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { managerLogf = orig })
	return &lines
}

// goodqa#15235 S5b condition A: the probe results observed on systemd 255
// become an explicit, logged refusal; no unit is launched on an old systemd.
func TestLinuxManagerRefusesUnsupportedHost(t *testing.T) {
	cases := []struct {
		name, version, preflight, want string
		launch                         bool
	}{
		{"systemd255", "systemd 255 (255.4-1ubuntu8.18)\n+PAM", "", "PrivatePIDs needs systemd>=257 (found 255); procfs isolation not enforced in user units", false},
		{"unparsable", "garbage", "", "cannot parse the systemd version", false},
		{"unknownAssignment", "systemd 257 (257.1)", "Unknown assignment: PrivatePIDs=yes", "PrivatePIDs is not supported", true},
		{"procSubsetIgnored", "systemd 257 (257.1)", "proc preflight: ProcSubset=pid is ineffective (/proc/uptime is visible)", "ProtectProc/ProcSubset are not enforced in user units", true},
		{"privatePIDsIgnored", "systemd 258 (258)", "proc preflight: PrivatePIDs is ineffective (launcher PID=7, want 1)", "PrivatePIDs was accepted but not enforced", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureManagerLog(t)
			var ran []string
			l := &LinuxManager{Self: "/nonexistent/fab", commandFn: fakeManagerCommands(t, c.version, c.preflight, &ran)}
			err := l.preflightManagerIsolation(1000)
			if !errors.Is(err, ErrLinuxManagerUnsupported) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrLinuxManagerUnsupported containing %q, got %v", c.want, err)
			}
			if len(*logs) != 1 || !strings.Contains((*logs)[0], c.want) {
				t.Fatalf("refusal not logged once with its reason: %q", *logs)
			}
			launched := strings.Contains(strings.Join(ran, " "), "systemd-run")
			if launched != c.launch {
				t.Fatalf("systemd-run launched=%v want %v (ran %v)", launched, c.launch, ran)
			}
		})
	}
}

// Other preflight failures keep the generic refusal: they are not reported as
// an unsupported host, and they are still refusals.
func TestLinuxManagerOtherPreflightFailureIsNotUnsupported(t *testing.T) {
	logs := captureManagerLog(t)
	var ran []string
	l := &LinuxManager{Self: "/nonexistent/fab", commandFn: fakeManagerCommands(t, "systemd 257 (257.1)", "manager socket preflight: /run/user/1000/bus is reachable", &ran)}
	err := l.preflightManagerIsolation(1000)
	if err == nil || errors.Is(err, ErrLinuxManagerUnsupported) {
		t.Fatalf("want a generic preflight refusal, got %v", err)
	}
	if len(*logs) != 0 {
		t.Fatalf("generic failure logged as unsupported: %q", *logs)
	}
}

func TestParseSystemdVersion(t *testing.T) {
	for in, want := range map[string]int{"systemd 255 (255.4-1ubuntu8.18)\n+PAM": 255, "systemd 257": 257, "": 0, "systemd x": 0, "foo 257": 0, "systemd -1": 0} {
		if got, _ := parseSystemdVersion(in); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}
