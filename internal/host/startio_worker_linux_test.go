//go:build linux

package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// promptFile makes the stdin file exactly as sched does (O_EXCL, 0600, owned
// by this uid) and reopens it read-only.
func promptFile(t *testing.T, dir string, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(dir, "prompt.jsonl")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Chmod(p, 0o600) //nolint:errcheck
	r, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func waitOrStop(t *testing.T, l *Linux, h Handle, d time.Duration) (int, error) {
	t.Helper()
	done := make(chan struct{})
	var code int
	var werr error
	go func() { code, werr = l.Wait(h); close(done) }()
	select {
	case <-done:
		return code, werr
	case <-time.After(d):
		l.Stop(h, "test timeout", time.Second) //nolint:errcheck
		t.Fatalf("the confined child did not end within %s", d)
		return 0, nil
	}
}

// goodqa#15373 AC-4 (review v5 B1): the launcher fabd uses, host.Linux with a
// worker account through the real sudoers rule, hands a 0600 regular file
// owned by fabd's uid to a child running as another uid. The child sees a
// regular file on fd 0 (no pty or relay pipe), cannot have opened it by path,
// and reads it through the inherited descriptor. Needs FABRIC_HOST_CONTROLS=1,
// FABRIC_FAB_BIN and FABRIC_WORKER_USER.
func TestStartIOWorkerFile(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" || os.Getenv("FABRIC_WORKER_USER") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 and FABRIC_WORKER_USER (starts a short unit as the worker)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	in := promptFile(t, g.RunDir, []byte("canary-15373-worker\n"))
	out, err := os.CreateTemp(g.RunDir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	os.Chmod(out.Name(), 0o666) //nolint:errcheck // the worker writes through the inherited fd anyway
	script := `stat -L -c %F /proc/self/fd/0; id -u; stat -L -c %u /proc/self/fd/0; cat`
	h, err := l.StartIO(g, []string{"/bin/sh", "-c", script}, nil, in, out, out)
	if err != nil {
		t.Fatal(err)
	}
	if code, werr := waitOrStop(t, l, h, 30*time.Second); werr != nil || code != 0 {
		b, _ := os.ReadFile(out.Name())
		t.Fatalf("wait: code %d err %v; output %q", code, werr, b)
	}
	b, _ := os.ReadFile(out.Name())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 4 {
		t.Fatalf("short output %q", b)
	}
	if lines[0] != "regular file" {
		t.Errorf("the child's stdin is %q, not a regular file (a pty or relay pipe was inserted)", lines[0])
	}
	if lines[2] != strconv.Itoa(os.Getuid()) {
		t.Errorf("stdin owner uid %s, want fabd's %d", lines[2], os.Getuid())
	}
	if lines[1] == lines[2] {
		t.Errorf("the child runs as the file owner (uid %s); this does not test the worker account", lines[1])
	}
	if lines[3] != "canary-15373-worker" {
		t.Errorf("the worker did not read the prompt through fd 0: %q", lines[3])
	}
}

// NEG-9: a child that never reads its stdin file exits and Wait returns.
func TestStartIOUnreadStdinDoesNotHang(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 (starts a short unit)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	in := promptFile(t, g.RunDir, make([]byte, 1<<20))
	out, err := os.CreateTemp(g.RunDir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h, err := l.StartIO(g, []string{"/bin/true"}, nil, in, out, out)
	if err != nil {
		t.Fatal(err)
	}
	if code, werr := waitOrStop(t, l, h, 30*time.Second); werr != nil || code != 0 {
		t.Fatalf("wait: code %d err %v", code, werr)
	}
}
