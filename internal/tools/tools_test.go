package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/host"
	"fabric/internal/tools"
	"fabric/internal/tools/fakehost"
)

var canary = "sk-" + "ant-CANARY-" + "0123456789abcdefXYZ"

type fixture struct {
	wt, run, outside string
	fh               *fakehost.Host
	conf             *tools.HostConfiner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{wt: filepath.Join(base, "wt"), run: filepath.Join(base, "run"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{f.wt, f.run, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.fh = fakehost.New()
	f.conf = &tools.HostConfiner{Host: f.fh, Base: host.Grant{Run: "r_tools1", Mission: "m_t1", Fence: 1, Model: "x", Effort: "x", Rung: "R0",
		TierTableSHA: "x", Profile: "P1", MaxTurns: 1, MaxBudgetUSD: 1, Wall: time.Minute, Worktree: f.wt, RunDir: f.run, Token: "tok-0123456789abcdef"}}
	return f
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func input(kv ...string) json.RawMessage {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

// NEG-35: the read tool cannot be led out of the worktree or made to hang.
func TestReadConfinement(t *testing.T) {
	f := newFixture(t)
	secret := filepath.Join(f.outside, "secret.txt")
	write(t, secret, "TOP-SECRET")
	write(t, filepath.Join(f.wt, "ok.txt"), "hello")
	write(t, filepath.Join(f.wt, "sub/dir/deep.txt"), "deep")
	write(t, filepath.Join(f.wt, "big.bin"), strings.Repeat("a", 5000))

	// Attack surface.
	os.Symlink(secret, filepath.Join(f.wt, "link-file"))
	os.Symlink(f.outside, filepath.Join(f.wt, "link-dir"))
	os.Symlink("/dev/zero", filepath.Join(f.wt, "link-zero"))
	os.Link(secret, filepath.Join(f.wt, "hard.txt"))
	if err := syscall.Mkfifo(filepath.Join(f.wt, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(f.wt, "adir"), 0o755)

	// Positive control: the same probes succeed against the target without the tool.
	if b, err := os.ReadFile(filepath.Join(f.wt, "link-file")); err != nil || string(b) != "TOP-SECRET" {
		t.Fatalf("control: symlink should read the secret unconfined (%v)", err)
	}

	r, err := tools.NewRead(f.wt, 1000, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]string{"ok.txt": "hello", "sub/dir/deep.txt": "deep", "./ok.txt": "hello", filepath.Join(f.wt, "ok.txt"): "hello"}
	for p, want := range good {
		if out, isErr := r.Run(context.Background(), input("path", p)); isErr || out != want {
			t.Errorf("read %q = %q err=%v, want %q", p, out, isErr, want)
		}
	}
	bad := []string{"link-file", "link-dir/secret.txt", "link-zero", "hard.txt", "fifo", "adir", "../outside/secret.txt",
		"sub/../../outside/secret.txt", secret, "/etc/passwd", "nope.txt", "ok.txt/x", "", "\x00", "."}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range bad {
			out, isErr := r.Run(context.Background(), input("path", p))
			if !isErr || strings.Contains(out, "TOP-SECRET") {
				t.Errorf("read %q was not refused: %q", p, out)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("read hung (FIFO or device opened?)")
	}
	// Byte bound.
	if out, _ := r.Run(context.Background(), input("path", "big.bin")); len(out) > 1100 || !strings.Contains(out, "truncated") {
		t.Errorf("size bound not applied: %d bytes", len(out))
	}
	if r.SideEffecting() {
		t.Error("read must not be side-effecting")
	}
}

func TestReadSwapRace(t *testing.T) {
	// A directory swapped for a symlink between calls is not followed.
	f := newFixture(t)
	write(t, filepath.Join(f.outside, "secret.txt"), "TOP-SECRET")
	os.Mkdir(filepath.Join(f.wt, "d"), 0o755)
	write(t, filepath.Join(f.wt, "d/secret.txt"), "inside")
	r, _ := tools.NewRead(f.wt, 1000, time.Second)
	if out, _ := r.Run(context.Background(), input("path", "d/secret.txt")); out != "inside" {
		t.Fatal(out)
	}
	os.RemoveAll(filepath.Join(f.wt, "d"))
	os.Symlink(f.outside, filepath.Join(f.wt, "d"))
	if out, isErr := r.Run(context.Background(), input("path", "d/secret.txt")); !isErr || strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("followed a swapped symlink: %q", out)
	}
}

func allow(pats ...string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range pats {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

const liveAllow = `^(ls|cat|wc|head|grep|echo|sort|env)( [A-Za-z0-9_./=-]+)*$`

func TestShell(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.wt, "a.txt"), "one\ntwo\n")
	t.Setenv("ANTHROPIC_API_KEY", canary) // the runner holds a key; the child must not
	t.Setenv("FAB_SECRET", canary)
	sh := &tools.Shell{C: f.conf, Allow: allow(liveAllow), Timeout: 5 * time.Second, MaxOutput: 200}
	if !sh.SideEffecting() {
		t.Fatal("shell must always be side-effecting")
	}
	if out, isErr := sh.Run(context.Background(), input("command", "wc -l a.txt")); isErr || !strings.Contains(out, "2") {
		t.Fatalf("wc: %q %v", out, isErr)
	}
	out, _ := sh.Run(context.Background(), input("command", "env"))
	if strings.Contains(out, canary) || strings.Contains(out, "ANTHROPIC_API_KEY") || strings.Contains(out, "FAB_SECRET") {
		t.Fatalf("runner environment leaked into the child: %q", out)
	}
	for _, s := range f.fh.Starts {
		for _, e := range s.Env {
			if strings.Contains(e, canary) {
				t.Fatalf("canary in child env: %s", e)
			}
		}
	}
	// Allowlist: metacharacters, other binaries, and the empty list are refused.
	for _, c := range []string{"ls; cat /etc/passwd", "cat a.txt | sh", "rm -rf .", "curl http://x", "ls $(id)", "ls `id`", "cat a.txt > b", "ls && ls"} {
		if o, isErr := sh.Run(context.Background(), input("command", c)); !isErr || !strings.Contains(o, "refused") {
			t.Errorf("command %q not refused: %q", c, o)
		}
	}
	none := &tools.Shell{C: f.conf}
	if _, isErr := none.Run(context.Background(), input("command", "ls")); !isErr {
		t.Error("empty allowlist allowed a command")
	}
	// Non-zero exit is reported as an error result.
	if o, isErr := sh.Run(context.Background(), input("command", "cat missing.txt")); !isErr || !strings.Contains(o, "exit status") {
		t.Errorf("exit status not surfaced: %q", o)
	}
	// Output cap.
	write(t, filepath.Join(f.wt, "long.txt"), strings.Repeat("x", 5000))
	if o, _ := sh.Run(context.Background(), input("command", "cat long.txt")); len(o) > 260 || !strings.Contains(o, "truncated") {
		t.Errorf("output cap not applied: %d", len(o))
	}
}

func TestShellTimeoutStopsChild(t *testing.T) {
	f := newFixture(t)
	sh := &tools.Shell{C: f.conf, Allow: allow(`^sleep [0-9]+$`), Timeout: 300 * time.Millisecond}
	start := time.Now()
	o, isErr := sh.Run(context.Background(), input("command", "sleep 31337"))
	if !isErr || !strings.Contains(o, "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: %q after %s", o, time.Since(start))
	}
	if out, err := exec.Command("pgrep", "-f", "sleep 31337").Output(); err == nil && len(out) > 0 {
		t.Fatalf("child still running: %s", out)
	}
}

func TestGrep(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.wt, "src/a.go"), "package a\n// TODO fix\n--help marker line\n")
	write(t, filepath.Join(f.outside, "secret.txt"), "TODO TOP-SECRET")
	os.Symlink(filepath.Join(f.outside, "secret.txt"), filepath.Join(f.wt, "src/link.txt"))
	os.Symlink(f.outside, filepath.Join(f.wt, "outdir"))
	g := &tools.Grep{C: f.conf, Root: f.wt, Timeout: 5 * time.Second, MaxOutput: 4000}
	if g.SideEffecting() {
		t.Fatal("grep must not be side-effecting")
	}
	out, isErr := g.Run(context.Background(), input("pattern", "TODO"))
	if isErr || !strings.Contains(out, "src/a.go:2") {
		t.Fatalf("grep: %q", out)
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("grep followed a symlink out of the worktree: %q", out)
	}
	// A pattern that looks like an option is data.
	if out, isErr := g.Run(context.Background(), input("pattern", "--help")); isErr || !strings.Contains(out, "src/a.go:3:--help marker line") {
		t.Fatalf("option-like pattern: %q %v", out, isErr)
	}
	if out, isErr := g.Run(context.Background(), input("pattern", "nothing-here-xyz")); isErr || out != "no matches" {
		t.Fatalf("no match: %q %v", out, isErr)
	}
	// F1: grep follows a symlink named as an operand, so the tool must refuse it itself.
	for _, p := range []string{"outdir", "src/link.txt"} {
		if o, isErr := g.Run(context.Background(), input("pattern", "TODO", "path", p)); !isErr || strings.Contains(o, "TOP-SECRET") {
			t.Errorf("symlink path %q was searched: %q", p, o)
		}
	}
	for _, p := range []string{"..", "../outside", "/etc", "src/../../x"} {
		if _, isErr := g.Run(context.Background(), input("pattern", "x", "path", p)); !isErr {
			t.Errorf("path %q not refused", p)
		}
	}
}

func TestRefuseUnadvertisedHost(t *testing.T) {
	if err := tools.RequireBoundary(tools.BoundaryFromControls("fake", nil)); err == nil {
		t.Error("no controls run, yet the boundary was accepted")
	}
	failed := []host.ControlResult{{Name: "network-denied", Passed: false}, {Name: "signal-denied", Passed: true}}
	if err := tools.RequireBoundary(tools.BoundaryFromControls("mac", failed)); err == nil {
		t.Error("failed control accepted")
	}
	pass := []host.ControlResult{{Name: "network-denied", Passed: true}, {Name: "signal-denied", Passed: true}}
	if err := tools.RequireBoundary(tools.BoundaryFromControls("mac", pass)); err != nil {
		t.Errorf("passing controls refused: %v", err)
	}
}

// AC-7 live half: the real host profile is exercised only with FABRIC_HOST_CONTROLS=1
// on a host that has the Slice 0 native profile. It is BLOCKED until Slice 0 lands.
func TestNativeToolsConfined(t *testing.T) {
	if os.Getenv("FABRIC_HOST_CONTROLS") != "1" {
		t.Skip("BLOCKED: needs the Slice 0 native host profile (FABRIC_HOST_CONTROLS=1); AC-7 live half is not claimed")
	}
	t.Skip("BLOCKED: Slice 0 native profile has not landed on public main")
}

// F4: an allowlist entry must match the whole command and no shell syntax passes.
func TestShellAllowlistIsAnchored(t *testing.T) {
	f := newFixture(t)
	sh := &tools.Shell{C: f.conf, Allow: allow(`^ls`, `^echo [a-z]+`), Timeout: 5 * time.Second}
	for _, c := range []string{"ls; cat /etc/passwd", "lsx", "ls foo | sh", "echo hi && id", "echo hi\nid", "ls $HOME", "echo a > b", "ls (x)"} {
		if o, isErr := sh.Run(context.Background(), input("command", c)); !isErr || !strings.Contains(o, "refused") {
			t.Errorf("command %q not refused: %q", c, o)
		}
	}
	if o, isErr := sh.Run(context.Background(), input("command", "echo hi")); isErr || !strings.Contains(o, "hi") {
		t.Errorf("plain command refused: %q %v", o, isErr)
	}
}

// F8: a child that floods its output is stopped long before the disk fills.
func TestShellOutputFloodStopped(t *testing.T) {
	f := newFixture(t)
	sh := &tools.Shell{C: f.conf, Allow: allow(`^yes$`), Timeout: 20 * time.Second, MaxOutput: 1000}
	start := time.Now()
	o, isErr := sh.Run(context.Background(), input("command", "yes"))
	if !isErr || time.Since(start) > 10*time.Second || len(o) > 1200 {
		t.Fatalf("flood not stopped: %d bytes, %s, err=%v", len(o), time.Since(start), isErr)
	}
}
