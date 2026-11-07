//go:build darwin

package host

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
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
		".docker/canary.txt":        "DOCKER-CANARY",
		".config/fabric/canary.txt": "FABRIC-CANARY",
		".pgpass":                   "PGPASS-CANARY",
		".gitconfig":                "[user]\n\tname = canary\n", // review A: git dies on a denied one
		"projects/wt/inside.txt":    "WT-OK",
		"projects/wt-sibling/x.txt": "SIBLING-CANARY",
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
	// Review F2: a second loopback service on an allowed port must be
	// reachable, so a profile that denies all network fails this test.
	okLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer okLn.Close()
	go http.Serve(okLn, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "EGRESS-OK") })) //nolint:errcheck
	// Review F4: a reviewed launch input under HOME is allowed back read-only.
	input := filepath.Join(home, "tools", "provider")
	if err := os.MkdirAll(filepath.Dir(input), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("INPUT-OK"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input+"-sibling", []byte("SIBLING-OK"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Darwin{TCPPorts: []int{443, okLn.Addr().(*net.TCPAddr).Port}, ExtraRO: []string{input}}
	prof := filepath.Join(t.TempDir(), "p.sb")
	text, err := d.profile(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prof, []byte(text), 0o600); err != nil {
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
	if got := run(nil, "/bin/cat", input); got != "INPUT-OK" {
		t.Errorf("worker cannot read a reviewed launch input under HOME: %q", got)
	}
	if got := run(nil, "/bin/cat", filepath.Join(home, "tools", "provider-sibling")); strings.Contains(got, "OK") {
		t.Errorf("allow-back of a launch input leaked a sibling")
	}
	if got := run(nil, "/bin/ls", filepath.Join(home, "projects")); strings.Contains(got, "wt-sibling") {
		t.Errorf("worker listed the directory holding other worktrees: %q", got)
	}
	// Review F3: path canonicalisation of the worktree under HOME works
	// (realpath(3) and git lstat every component).
	wt := filepath.Join(home, "projects", "wt")
	if got := run(nil, "/bin/realpath", wt); got != realPath(wt) {
		t.Errorf("realpath of the worktree under the profile: %q", got)
	}
	if out, err := exec.Command("/usr/bin/git", "-C", wt, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	// Review A (716fc04): git runs with the production worker environment,
	// where HOME is the real (denied) home, not with HOME moved aside.
	wenv, err := darwinEnv(g, workerGitEnv(g))
	if err != nil {
		t.Fatal(err)
	}
	if got := run(wenv, "/usr/bin/git", "-C", wt, "rev-parse", "--show-toplevel"); got != realPath(wt) {
		t.Errorf("git rev-parse in a worktree under HOME failed under the profile with the worker env: %q", got)
	}

	// Review F1: the account's own home is denied even though HOME points
	// elsewhere (here a temp dir), and other accounts' homes are too.
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	acct := filepath.Join(u.HomeDir, fmt.Sprintf(".fab-k3-acct-%d", time.Now().UnixNano()))
	if err := os.WriteFile(acct, []byte("ACCT-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(acct)
	if got := run(nil, "/bin/cat", acct); strings.Contains(got, "CANARY") {
		t.Error("worker read the account's home while HOME was overridden")
	}
	if got := run(nil, "/bin/ls", "/Users"); strings.Contains(got, filepath.Base(u.HomeDir)) {
		t.Errorf("worker listed /Users: %q", got)
	}
	if got := run(nil, "/usr/bin/curl", "-s", "--max-time", "3", "http://"+okLn.Addr().String()+"/"); got != "EGRESS-OK" {
		t.Errorf("worker could not reach a service on an allowed port: %q", got)
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

// workerGitEnv is the part of the claude launch env (internal/provider/
// claude/launch.go) that decides where git looks for configuration.
func workerGitEnv(g Grant) []string {
	return []string{
		"CLAUDE_CONFIG_DIR=" + g.RunDir + "/claude-config",
		"XDG_CACHE_HOME=" + g.RunDir + "/cache",
		"XDG_CONFIG_HOME=" + g.RunDir + "/config",
		"GIT_CONFIG_GLOBAL=" + g.RunDir + "/config/git/config",
	}
}

// Review F1: an empty, relative or root HOME refuses the profile (and so
// the launch) instead of silently dropping the home deny.
func TestDarwinProfileRefusesBadHome(t *testing.T) {
	g := controlGrant(t.TempDir())
	for _, h := range []string{"", "relative/home", "/"} {
		t.Setenv("HOME", h)
		if _, err := (&Darwin{}).profile(g); err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Errorf("HOME=%q: profile built or refused for another reason: %v", h, err)
		}
		if _, err := (&Darwin{SandboxExec: "/usr/bin/sandbox-exec", AllowP1: true}).Start(g, []string{"/usr/bin/true"}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Errorf("HOME=%q: Start launched or refused for another reason: %v", h, err)
		}
	}
}

// Review F4 live: a reviewed launch input under the account's home passes
// launch_inputs_readable, and the same file without the allow-back fails
// it (so the control is not vacuous). Opt-in like the other controls.
func TestDarwinLaunchInputsControl(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") == "" {
		t.Skip("set FABRIC_HOST_CONTROLS=1 to run the process-signalling controls")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(u.HomeDir, ".fab-nc-input-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "provider")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := func(d *Darwin) ControlResult {
		res, err := d.NegativeControls(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Name == "launch_inputs_readable" {
				return r
			}
		}
		t.Fatal("launch_inputs_readable did not run")
		return ControlResult{}
	}
	if r := result(&Darwin{SandboxExec: "/usr/bin/sandbox-exec", ExtraRO: []string{in}}); !r.Passed {
		t.Errorf("allowed-back input unreadable: %s", r.Detail)
	}
	// Control: an input the profile does not allow back is reported denied.
	g := controlGrant(t.TempDir())
	bare := &Darwin{SandboxExec: "/usr/bin/sandbox-exec", AllowP1: true}
	res, err := bare.runControl(g, fmt.Sprintf(`head -c 1 %q >/dev/null 2>&1 && echo in=read || echo in=denied`, in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "in=denied") {
		t.Errorf("control: an input under the home read without an allow-back: %q", res)
	}

	// Review 716fc04 C: the provider must start, not only be readable. A
	// script provider whose interpreter sits in an unlisted home directory
	// (the nvm/npm case) is readable but fails provider_starts_under_profile.
	starts := func(d *Darwin) ControlResult {
		res, err := d.NegativeControls(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Name == "provider_starts_under_profile" {
				return r
			}
		}
		t.Fatal("provider_starts_under_profile did not run")
		return ControlResult{}
	}
	tools := filepath.Join(dir, "tools")
	interp := filepath.Join(dir, "interp")
	for _, p := range []string{tools, interp} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	good := filepath.Join(tools, "provider")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho v1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(interp, "fakenode")); err != nil {
		t.Fatal(err)
	}
	nvm := filepath.Join(tools, "npm-provider")
	if err := os.WriteFile(nvm, []byte("#!"+filepath.Join(interp, "fakenode")+"\necho v1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if r := starts(&Darwin{SandboxExec: "/usr/bin/sandbox-exec", ExtraRO: []string{tools}, Provider: good}); !r.Passed {
		t.Errorf("an allowed-back provider did not start: %s", r.Detail)
	}
	if r := starts(&Darwin{SandboxExec: "/usr/bin/sandbox-exec", ExtraRO: []string{tools}, Provider: nvm}); r.Passed {
		t.Errorf("a provider whose interpreter is unreadable passed: %s", r.Detail)
	}
}

// Review F5: with a work root outside every home, the directories holding
// other runs and other missions' worktrees are denied before the allow-back.
// Every writable place on a test Mac is a home or a temp root, so this
// checks the generated profile text; inside a temp root it is skipped.
func TestDarwinProfileDeniesSiblingRuns(t *testing.T) {
	g := controlGrant(t.TempDir())
	g.Worktree, g.RunDir = "/opt/fabric-k3/wt/m1", "/opt/fabric-k3/run/a1"
	text, err := (&Darwin{}).profile(g)
	if err != nil {
		t.Fatal(err)
	}
	deny := text[strings.Index(text, "(deny file-read*"):strings.Index(text, "(allow file-read*")]
	for _, p := range []string{`(subpath "/opt/fabric-k3/wt")`, `(subpath "/opt/fabric-k3/run")`, `(subpath "/Users")`} {
		if !strings.Contains(deny, p) {
			t.Errorf("read deny lacks %s:\n%s", p, deny)
		}
	}
	// An input that is a home, holds one, is another account's home, holds
	// the directory of other runs, or holds a secret path is refused.
	home := os.Getenv("HOME")
	for _, in := range []string{home, "/Users", "/", "relative", "/Users/someone-else", "/opt/fabric-k3", "/opt/fabric-k3/wt",
		filepath.Join(home, ".config"), filepath.Join(home, "Library")} {
		if _, err := (&Darwin{ExtraRO: []string{in}}).profile(g); err == nil {
			t.Errorf("read-only input %q accepted", in)
		}
	}
	// A tool's own directory is accepted.
	if _, err := (&Darwin{ExtraRO: []string{filepath.Join(home, ".local", "share", "claude", "versions"), "/opt/fabric-k3/checkpoint.schema.json"}}).profile(g); err != nil {
		t.Errorf("a tool directory was refused: %v", err)
	}
	g = controlGrant(t.TempDir())
	if text, err = (&Darwin{}).profile(g); err != nil {
		t.Fatal(err)
	}
	deny = text[strings.Index(text, "(deny file-read*"):strings.Index(text, "(allow file-read*")]
	if strings.Contains(deny, "/var/folders") || strings.Contains(deny, "/private/tmp") {
		t.Errorf("read deny covers a shared temp root:\n%s", deny)
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
