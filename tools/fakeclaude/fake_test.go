// goodqa#15373 D1 AC-9: the stdin-checking fake runs the recorded script
// only when the prompt arrives on stdin as one stream-json user message
// carrying this mission's projection, and only through the recorded fake
// with its recorded sha256.
package fakeclaude_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"fabric/internal/provider/claude"
)

const stdinFailure = "stdin prompt missing or wrong"

func needTools(t *testing.T) {
	t.Helper()
	for _, p := range []string{"/bin/sh", "/usr/bin/jq"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("no %s", p)
		}
	}
}

// runIn runs the stdin fake at dir/fakeclaude-stdin.sh with stdin from f.
func runIn(t *testing.T, dir string, stdin *os.File, mission string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dir, "fakeclaude-stdin.sh")}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "FAB_MISSION=" + mission, "FAB_WORKTREE=" + t.TempDir(), "FAB_BIN=/nonexistent/fab"}
	cmd.Stdin = stdin
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake exited with %v: %s", err, out)
	}
	return string(out)
}

func stdinFile(t *testing.T, s string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompt.jsonl")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func run(t *testing.T, stdin string, mission string, args ...string) string {
	t.Helper()
	here, _ := filepath.Abs(".")
	return runIn(t, here, stdinFile(t, stdin), mission, args...)
}

func promptJSON(t *testing.T, content string) string {
	t.Helper()
	b, err := claude.StreamInput(content)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func prompt(t *testing.T, mission string) string {
	t.Helper()
	return promptJSON(t, `{"builder":"b","checkpoint":{"mission":"`+mission+`","version":{}},"window":[]}`)
}

var stream = []string{"-p", "--input-format", "stream-json"}

func blockedStdin(out string) bool {
	return strings.Contains(out, stdinFailure) && strings.Contains(out, `"kind":"blocked"`)
}

func TestStdinFake(t *testing.T) {
	needTools(t)
	good := run(t, prompt(t, "m_t1"), "m_t1", stream...)
	if strings.Contains(good, stdinFailure) || !strings.Contains(good, `"type":"result"`) {
		t.Errorf("a correct stdin prompt was refused:\n%s", good)
	}
	bad := map[string]struct {
		stdin, mission string
		args           []string
	}{
		"empty stdin":        {"", "m_t1", stream},
		"two messages":       {prompt(t, "m_t1") + prompt(t, "m_t1"), "m_t1", stream},
		"wrong mission":      {prompt(t, "m_other"), "m_t1", stream},
		"not a user message": {`{"type":"assistant","message":{"content":"{\"checkpoint\":{\"mission\":\"m_t1\"}}"}}` + "\n", "m_t1", stream},
		// review v6 N-b: the id elsewhere in the content does not count
		"mission only outside checkpoint": {promptJSON(t, `{"checkpoint":{"mission":"m_other"},"window":["{\"mission\":\"m_t1\"}"]}`), "m_t1", stream},
		"content not json":                {promptJSON(t, `"mission":"m_t1"`), "m_t1", stream},
		"prompt only in argv":             {"", "m_t1", []string{"-p", prompt(t, "m_t1")}},
		"no mission":                      {prompt(t, "m_t1"), "", stream},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if out := run(t, c.stdin, c.mission, c.args...); !blockedStdin(out) {
				t.Errorf("want the stdin-failure blocked end, got:\n%s", out)
			}
		})
	}
}

// Review v5 B1: the worker reads its inherited fd 0; a file it cannot reopen
// by path (a 0600 file of another uid, here mode 000 for this uid) still works.
func TestStdinFakeReadsInheritedFD(t *testing.T) {
	needTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root reopens a mode-000 file")
	}
	f := stdinFile(t, prompt(t, "m_t1"))
	if err := os.Chmod(f.Name(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Open(f.Name()); err == nil {
		t.Fatal("the stdin file can still be opened by path; the test proves nothing")
	}
	here, _ := filepath.Abs(".")
	if out := runIn(t, here, f, "m_t1", stream...); strings.Contains(out, stdinFailure) {
		t.Errorf("a prompt on an inherited, non-reopenable fd was refused:\n%s", out)
	}
}

// Review v6 N-a, v7 F5: the fake delegates only to the recorded fake, by sha.
func TestStdinFakeDelegatePinned(t *testing.T) {
	needTools(t)
	here, _ := filepath.Abs(".")
	src, err := os.ReadFile(filepath.Join(here, "fakeclaude-stdin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := os.ReadFile(filepath.Join(here, "fakeclaude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^DELEGATE_SHA=([0-9a-f]{64})$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no DELEGATE_SHA line in fakeclaude-stdin.sh")
	}
	sum := sha256.Sum256(rec)
	if string(m[1]) != hex.EncodeToString(sum[:]) {
		t.Fatalf("DELEGATE_SHA %s is not the sha256 of fakeclaude.sh", m[1])
	}
	inst, err := os.ReadFile(filepath.Join(here, "..", "fabd-install", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inst), "\nFAKE_SHA="+string(m[1])+"\n") {
		t.Errorf("DELEGATE_SHA differs from install.sh FAKE_SHA, the live guard's pin")
	}
	layout := func(t *testing.T, files map[string][]byte) string {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "fakeclaude-stdin.sh"), src, 0o755)
		for n, b := range files {
			os.WriteFile(filepath.Join(d, n), b, 0o755)
		}
		return d
	}
	t.Run("a changed delegate is refused", func(t *testing.T) {
		d := layout(t, map[string][]byte{"fakeclaude.sh": append(rec, []byte("\n# changed\n")...)})
		if out := runIn(t, d, stdinFile(t, prompt(t, "m_t1")), "m_t1", stream...); !blockedStdin(out) || !strings.Contains(out, "recorded sha256") {
			t.Errorf("a delegate with another sha ran:\n%s", out)
		}
	})
	t.Run("fakeclaude-live is preferred and must be pinned too", func(t *testing.T) {
		d := layout(t, map[string][]byte{"fakeclaude-live": []byte("#!/bin/sh\necho unpinned\n"), "fakeclaude.sh": rec})
		if out := runIn(t, d, stdinFile(t, prompt(t, "m_t1")), "m_t1", stream...); !blockedStdin(out) || strings.Contains(out, "unpinned") {
			t.Errorf("an unpinned fakeclaude-live ran or was skipped for fakeclaude.sh:\n%s", out)
		}
	})
	t.Run("no delegate", func(t *testing.T) {
		d := layout(t, nil)
		if out := runIn(t, d, stdinFile(t, prompt(t, "m_t1")), "m_t1", stream...); !blockedStdin(out) {
			t.Errorf("want the blocked end without a delegate:\n%s", out)
		}
	})
}

// The file must be executable for exec; keep the repo copy that way.
func TestStdinFakeExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	for _, n := range []string{"fakeclaude-stdin.sh", "fakeclaude.sh"} {
		fi, err := os.Stat(n)
		if err != nil || fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", n)
		}
	}
}
