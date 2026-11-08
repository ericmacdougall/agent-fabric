//go:build linux

package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goodqa#15177: no environment value a worker gets (the runner login, the
// run token, anything) may appear on a command line. sudo and systemd-run
// live for the whole run, and /proc/<pid>/cmdline is world-readable.
func TestStartArgsCarryNoValues(t *testing.T) {
	g := controlGrant(t.TempDir())
	l := &Linux{Self: "/srv/fabric/bin/fab"}
	args := workerArgs(t, l, g, []string{"FABRIC_DSN=canary-runner-login-15177", "FAB_MISSION=m_canary15177"})
	for _, a := range args {
		for _, secret := range []string{"canary-runner-login-15177", "m_canary15177", g.Token} {
			if strings.Contains(a, secret) {
				t.Fatalf("argument %q carries a worker environment value", a)
			}
		}
	}
}

// TestWorkerEnvNotOnCommandLine is the live control: a real worker gets
// its FABRIC_DSN, while no process on the host shows it in its command
// line. Opt-in (starts a 3 s unit as the worker user): FABRIC_HOST_CONTROLS=1,
// FABRIC_FAB_BIN, FABRIC_WORKER_USER, FABRIC_WORKER_ROOT.
func TestWorkerEnvNotOnCommandLine(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 (starts a 3 s unit as the worker user)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	// Built at run time so no other command line (a shell, a log filter)
	// can carry it by accident.
	canary := fmt.Sprintf("cnry%dx%d", os.Getpid(), time.Now().UnixNano())
	out, err := os.Create(filepath.Join(g.RunDir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h, err := l.Start(g, []string{"sh", "-c", "printenv FABRIC_DSN; sleep 3"}, []string{"FABRIC_DSN=" + canary}, out, out)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	seen := 0
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			continue
		}
		seen++
		if bytes.Contains(b, []byte(canary)) {
			t.Errorf("%s shows the worker's FABRIC_DSN: %q", p, bytes.ReplaceAll(b, []byte{0}, []byte{' '}))
		}
	}
	if seen < 5 {
		t.Fatalf("read only %d command lines: the scan is not looking", seen)
	}
	if _, err := l.Wait(h); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(g.RunDir, "out.txt"))
	if !strings.Contains(string(got), canary) {
		t.Fatalf("the worker did not get its FABRIC_DSN (output %q)", got)
	}
	left, _ := filepath.Glob(filepath.Join(g.RunDir, ".fab-env-*"))
	if len(left) != 0 {
		t.Fatalf("environment file left after the run: %v", left)
	}
}
