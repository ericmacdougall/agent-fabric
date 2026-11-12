// goodqa#15373 D1 AC-9: the stdin-checking fake runs the recorded script
// only when the prompt arrives on stdin as one stream-json user message
// carrying this mission's projection.
package fakeclaude_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/provider/claude"
)

const stdinFailure = "stdin prompt missing or wrong"

func run(t *testing.T, stdin string, mission string, args ...string) string {
	t.Helper()
	script, _ := filepath.Abs("fakeclaude-stdin.sh")
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "FAB_MISSION=" + mission, "FAB_WORKTREE=" + t.TempDir(), "FAB_BIN=/nonexistent/fab"}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake exited with %v: %s", err, out)
	}
	return string(out)
}

func prompt(t *testing.T, mission string) string {
	t.Helper()
	b, err := claude.StreamInput(`{"checkpoint":{"mission":"` + mission + `","version":{}},"next":"continue"}`)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStdinFake(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	stream := []string{"-p", "--input-format", "stream-json"}
	good := run(t, prompt(t, "m_t1"), "m_t1", stream...)
	if strings.Contains(good, stdinFailure) || !strings.Contains(good, `"type":"result"`) {
		t.Errorf("a correct stdin prompt was refused:\n%s", good)
	}
	bad := map[string]struct {
		stdin, mission string
		args           []string
	}{
		"empty stdin":         {"", "m_t1", stream},
		"two messages":        {prompt(t, "m_t1") + prompt(t, "m_t1"), "m_t1", stream},
		"wrong mission":       {prompt(t, "m_other"), "m_t1", stream},
		"not a user message":  {`{"type":"assistant","message":{"content":"{\"mission\":\"m_t1\"}"}}` + "\n", "m_t1", stream},
		"prompt only in argv": {"", "m_t1", []string{"-p", prompt(t, "m_t1")}},
		"no mission":          {prompt(t, "m_t1"), "", stream},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			out := run(t, c.stdin, c.mission, c.args...)
			if !strings.Contains(out, stdinFailure) || !strings.Contains(out, `"kind":"blocked"`) {
				t.Errorf("want the stdin-failure blocked end, got:\n%s", out)
			}
		})
	}
}
