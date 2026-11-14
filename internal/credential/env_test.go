package credential

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func fakeBinary(t *testing.T, content string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	must(t, os.WriteFile(p, []byte(content), 0o755))
	sha, err := FileSHA256(p)
	must(t, err)
	return p, sha
}

func TestClaudeEnvManifest(t *testing.T) {
	tok := synthToken(t, 108)
	bin, sha := fakeBinary(t, "#!/bin/sh\necho real claude stand-in\n")
	base := []string{"CLAUDE_CONFIG_DIR=/run/x/claude-config", "FABRIC_DSN=postgres://u:p@h/db", "FABRIC_HOME=/run/x/fabric-home",
		"ANTHROPIC_API_KEY=x", "HOME=/home/eric", "FAB_RUN=t"}
	env, names, err := ClaudeEnv("claude-code", bin, sha, base, "/run/x/home", NewSecret(KindToken, "claude-oauth.v0", tok))
	must(t, err)
	sort.Strings(names)
	want := []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_SKIP_PROMPT_HISTORY",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB", "CLAUDE_CONFIG_DIR", "DISABLE_AUTOUPDATER", "DISABLE_ERROR_REPORTING",
		"DISABLE_TELEMETRY", "FAB_RUN", "HOME"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names %v", names)
	}
	for _, p := range env {
		if strings.HasPrefix(p, "HOME=") && p != "HOME=/run/x/home" {
			t.Fatalf("HOME not per-run: %s", p[:5])
		}
		for _, bad := range []string{"FABRIC_DSN=", "FABRIC_HOME=", "ANTHROPIC_API_KEY="} {
			if strings.HasPrefix(p, bad) {
				t.Fatalf("%s reached the claude env", bad)
			}
		}
	}
	must(t, CheckManifest("claude-code", bin, sha, env))
}

// NEG-11: a fake or non-claude launch that would carry the token refuses;
// ANTHROPIC_API_KEY in the manifest fails.
func TestTokenNeverReachesFake(t *testing.T) {
	tok := synthToken(t, 108)
	s := NewSecret(KindToken, "claude-oauth.v0", tok)
	real, sha := fakeBinary(t, "real")
	fake, _ := fakeBinary(t, "fake claude")
	cases := []struct {
		provider, bin, pin string
	}{
		{"claude-code", fake, sha}, // swapped binary
		{"fake", real, sha},        // wrong provider
		{"anthropic-api", real, sha},
		{"claude-code", real, ""}, // no pin
		{"claude-code", filepath.Join(t.TempDir(), "missing"), sha},
	}
	for _, c := range cases {
		_, _, err := ClaudeEnv(c.provider, c.bin, c.pin, nil, "/run/h", s)
		if !errors.Is(err, ErrTokenLaunch) || strings.Contains(err.Error(), string(tok[:12])) {
			t.Fatalf("%+v: %v", c, err)
		}
		if err := CheckManifest(c.provider, c.bin, c.pin, []string{"CLAUDE_CODE_OAUTH_TOKEN=" + string(tok)}); !errors.Is(err, ErrTokenLaunch) {
			t.Fatalf("manifest %+v: %v", c, err)
		}
	}
	if err := CheckManifest("claude-code", real, sha, []string{"ANTHROPIC_API_KEY=x"}); err == nil {
		t.Fatal("ANTHROPIC_API_KEY passed")
	}
	if err := CheckManifest("fake", fake, "", []string{"FAB_RUN=x"}); err != nil {
		t.Fatalf("a token-free fake launch refused: %v", err)
	}
	// A symlinked binary is not followed for the pin.
	link := filepath.Join(t.TempDir(), "claude")
	must(t, os.Symlink(real, link))
	if _, _, err := ClaudeEnv("claude-code", link, sha, nil, "/run/h", s); !errors.Is(err, ErrTokenLaunch) {
		t.Fatalf("symlinked binary: %v", err)
	}
}

// AC-15: O_EXCL stream descriptors, never through a symlink, mode 0600.
func TestOpenStreamExclusive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stream.jsonl")
	f, err := OpenStream(p)
	must(t, err)
	f.WriteString("a\n")
	f.Close()
	if fi, _ := os.Lstat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := OpenStream(p); err == nil {
		t.Fatal("existing stream reopened")
	}
	victim := filepath.Join(dir, "victim")
	must(t, os.WriteFile(victim, []byte("keep"), 0o600))
	l := filepath.Join(dir, "stderr.log")
	must(t, os.Symlink(victim, l))
	if _, err := OpenStream(l); err == nil {
		t.Fatal("opened through a symlink")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("victim changed")
	}
}
