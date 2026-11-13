//go:build linux

package host

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// goodqa#15373 D1 AC-4: StartIO connects stdin through the same confined
// transient unit Start uses. A real unit echoes stdin back and is gone after
// Wait. Opt-in like the other host controls (starts a short unit):
// FABRIC_HOST_CONTROLS=1 and FABRIC_FAB_BIN (FABRIC_WORKER_USER optional).
func TestStartIOEchoLinux(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 (starts a short unit)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(g.RunDir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h, err := l.StartIO(g, []string{"/bin/cat"}, nil, r, out, out)
	r.Close() // the child holds its own copy
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("hello-15373\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	done := make(chan struct{})
	var code int
	var werr error
	go func() { code, werr = l.Wait(h); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		l.Stop(h, "test timeout", time.Second) //nolint:errcheck
		t.Fatal("the confined cat did not end after stdin closed")
	}
	if werr != nil || code != 0 {
		t.Fatalf("wait: code %d err %v", code, werr)
	}
	b, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(b), "hello-15373") {
		t.Fatalf("stdin did not reach the confined child; output %q", b)
	}
	st, _ := exec.Command("systemctl", "--user", "is-active", h.Unit).Output()
	if s := strings.TrimSpace(string(st)); s == "active" || s == "activating" {
		t.Errorf("unit %s still %s after Wait", h.Unit, s)
	}
}

// A nil stdin keeps Start's behaviour: the child reads EOF at once, so a
// confined cat ends immediately with no output.
func TestStartIONilStdinIsStart(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 (starts a short unit)")
	}
	l := testProfile(t).(*Linux)
	g := controlGrant(t.TempDir())
	out, err := os.CreateTemp(g.RunDir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h, err := l.StartIO(g, []string{"/bin/cat"}, nil, nil, out, out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := l.Wait(h); err != nil || code != 0 {
		t.Fatalf("wait: code %d err %v", code, err)
	}
	if b, _ := os.ReadFile(out.Name()); len(strings.TrimSpace(string(b))) != 0 {
		t.Errorf("cat with no stdin printed %q", b)
	}
}
