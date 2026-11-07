//go:build darwin

package host

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// goodqa#15161 (Kimi K3) and red-team F4: a Darwin worker runs as fabd's
// own uid, so the Seatbelt profile is the only boundary. It must deny
// reads anywhere under HOME except the run's own directories (a deny list
// always misses something: .docker, .pgpass, ~/.config/fabric), deny
// reading other processes' arguments and environment, and deny network
// except outbound TCP to the allowed ports (no loopback services on other
// ports, no unix sockets such as an ssh-agent).
func TestDarwinProfileDefaultDeny(t *testing.T) {
	if os.Getenv("FAB_K3_HELPER") != "" {
		return // helper re-exec of this binary: only TestDarwinProcArgsHelper acts
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for p, body := range map[string]string{
		".docker/canary.txt":         "DOCKER-CANARY",
		".config/fabric/canary.txt":  "FABRIC-CANARY",
		".pgpass":                    "PGPASS-CANARY",
		"projects/wt/inside.txt":     "WT-OK",
		"projects/wt-sibling/x.txt":  "SIBLING-CANARY",
	} {
		full := filepath.Join(home, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := controlGrant(t.TempDir())
	g.Worktree = filepath.Join(home, "projects", "wt") // a worktree under HOME stays readable
	d := &Darwin{TCPPorts: []int{443}}
	prof := filepath.Join(t.TempDir(), "p.sb")
	if err := os.WriteFile(prof, []byte(d.profile(g)), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env []string, argv ...string) string {
		cmd := exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", prof}, argv...)...)
		cmd.Env = env
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	for _, p := range []string{".docker/canary.txt", ".config/fabric/canary.txt", ".pgpass", "projects/wt-sibling/x.txt"} {
		if got := run(nil, "/bin/cat", filepath.Join(home, p)); strings.Contains(got, "CANARY") {
			t.Errorf("worker read HOME/%s", p)
		}
	}
	if got := run(nil, "/bin/cat", filepath.Join(home, "projects", "wt", "inside.txt")); got != "WT-OK" {
		t.Errorf("worker cannot read its own worktree under HOME: %q", got)
	}

	// Loopback service on a port that is not allowed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "EGRESS-OK") })) //nolint:errcheck
	if got := run(nil, "/usr/bin/curl", "-s", "--max-time", "3", "http://"+ln.Addr().String()+"/"); got == "EGRESS-OK" {
		t.Error("worker reached a loopback service on a port that is not allowed")
	}
	// A unix socket outside HOME (an agent socket lives in a temp dir).
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fab-k3-%d.sock", time.Now().UnixNano()))
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	defer os.Remove(sock)
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "SOCKET-OK") //nolint:errcheck
			c.Close()
		}
	}()
	if got := run(nil, "/usr/bin/nc", "-U", sock); strings.Contains(got, "SOCKET-OK") {
		t.Error("worker connected to a unix socket")
	}

	// Another process's environment (fabd's FABRIC_DSN on a real host).
	// The target is this test binary (not an Apple platform binary, whose
	// environment macOS hides anyway), so the control is meaningful.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	canary := fmt.Sprintf("cnry%dx%d", os.Getpid(), time.Now().UnixNano())
	other := exec.Command(self, "-test.run=^TestDarwinProcArgsHelper$")
	other.Env = []string{"FAB_K3_HELPER=sleep", "FABRIC_DSN=" + canary}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill() //nolint:errcheck
	time.Sleep(300 * time.Millisecond)
	// Control: unsandboxed, the same uid reads it (so the probe can see).
	if b, err := unix.SysctlRaw("kern.procargs2", other.Process.Pid); err != nil || !strings.Contains(string(b), canary) {
		t.Fatalf("control: procargs2 of the target unreadable even unsandboxed (%v)", err)
	}
	got := run([]string{"FAB_K3_HELPER=procargs", "FAB_K3_PID=" + strconv.Itoa(other.Process.Pid)}, self, "-test.run=^TestDarwinProcArgsHelper$")
	if strings.Contains(got, canary) {
		t.Error("worker read another process's environment")
	}
	if !strings.Contains(got, "helper-ran") {
		t.Fatalf("procargs helper did not run under the profile: %q", got)
	}
}

// TestDarwinProcArgsHelper is the helper the test above runs under
// sandbox-exec: it prints another process's arguments and environment
// (kern.procargs2), or the error.
func TestDarwinProcArgsHelper(t *testing.T) {
	switch os.Getenv("FAB_K3_HELPER") {
	case "sleep":
		time.Sleep(20 * time.Second)
		return
	case "procargs":
	default:
		t.Skip("helper only")
	}
	pid, _ := strconv.Atoi(os.Getenv("FAB_K3_PID"))
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	fmt.Printf("helper-ran err=%v\n", err)
	os.Stdout.Write(b) //nolint:errcheck
}
