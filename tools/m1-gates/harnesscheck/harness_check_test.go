package harnesscheck

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude emulates the CLI: it refuses a writable working directory,
// runs ./probe.sh (scrubbing the token from the child when scrub is set),
// optionally writes a transcript or echoes the token, and prints stream-json.
func fakeClaude(scrub, transcript, echoToken bool) string {
	s := "#!/bin/bash\n"
	s += "if touch ./w 2>/dev/null; then echo writable >&2; exit 3; fi\n"
	if scrub {
		s += "out=$(env -u CLAUDE_CODE_OAUTH_TOKEN ./probe.sh)\n"
	} else {
		// One child scrubbed, one not: only the presence count can catch it.
		s += "out=\"$(env -u CLAUDE_CODE_OAUTH_TOKEN ./probe.sh) $(./probe.sh)\"\n"
	}
	if transcript {
		s += "mkdir -p \"$CLAUDE_CONFIG_DIR/projects/x\" && echo '{}' > \"$CLAUDE_CONFIG_DIR/projects/x/s.jsonl\"\n"
	}
	if echoToken {
		s += "echo \"debug $CLAUDE_CODE_OAUTH_TOKEN\" >&2\n"
	}
	s += `echo '{"type":"system","subtype":"init","session_id":"s"}'` + "\n"
	s += `echo "{\"type\":\"user\",\"tool_result\":\"$out\"}"` + "\n"
	s += `echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"$out\"}"` + "\n"
	return s
}

func run(t *testing.T, script string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	tokb := make([]byte, 48)
	rand.Read(tokb)
	tok := "sk-" + "ant-oat01-" + hex.EncodeToString(tokb)
	tf := filepath.Join(dir, "claude-oauth")
	if err := os.WriteFile(tf, []byte(tok), 0o640); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../harness-check.sh", "--claude", bin, "--sha", hex.EncodeToString(sum[:]), "--token-file", tf)
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), tok[13:40]) {
		t.Fatal("the script printed the token")
	}
	return string(out), cmd.ProcessState.ExitCode()
}

func TestHarnessCheckPassesAScrubbingCLI(t *testing.T) {
	out, code := run(t, fakeClaude(true, false, false))
	if code != 0 || !strings.Contains(out, "PASS") || !strings.Contains(out, "token_var_present=0") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestHarnessCheckFailsEachBrokenCLI(t *testing.T) {
	for name, script := range map[string]string{
		"child sees token":   fakeClaude(false, false, false),
		"transcript written": fakeClaude(true, true, false),
		"token in output":    fakeClaude(true, false, true),
	} {
		out, code := run(t, script)
		if code != 1 || strings.Contains(out, "PASS") {
			t.Fatalf("%s: exit %d: %s", name, code, out)
		}
	}
	// A binary that is not the pinned one is an environment error.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "t"), []byte("x"), 0o600)
	cmd := exec.Command("bash", "../harness-check.sh", "--claude", bin, "--sha", strings.Repeat("0", 64), "--token-file", filepath.Join(dir, "t"))
	cmd.Run()
	if cmd.ProcessState.ExitCode() != 2 {
		t.Fatalf("sha mismatch exit %d", cmd.ProcessState.ExitCode())
	}
}
