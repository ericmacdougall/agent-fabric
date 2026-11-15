package credential

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Environment names (AC-14).
const (
	EnvOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvAPIKey     = "ANTHROPIC_API_KEY"
)

// ClaudeFixedEnv is set on every claude-code M1 worker (AC-14).
var ClaudeFixedEnv = []string{
	"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
	"CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	"DISABLE_TELEMETRY=1",
	"DISABLE_ERROR_REPORTING=1",
	"DISABLE_AUTOUPDATER=1",
}

// claudeStripped never reaches a claude-code worker (AC-14).
var claudeStripped = map[string]bool{"FABRIC_DSN": true, "FABRIC_HOME": true, EnvAPIKey: true, EnvOAuthToken: true}

// ErrTokenLaunch refuses a launch that would carry the token anywhere but a
// pinned real claude.
var ErrTokenLaunch = errors.New("credential: launch refused: CLAUDE_CODE_OAUTH_TOKEN goes only to a provider=claude-code launch of the pinned real claude")

// FileSHA256 hashes a file opened without following a final symlink.
func FileSHA256(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ClaudeEnv builds the claude-code M1 worker environment from the launch's
// base environment (AC-14): FABRIC_DSN, FABRIC_HOME, ANTHROPIC_API_KEY and
// any inherited token are removed; the fixed variables, HOME (a per-run
// directory) and CLAUDE_CODE_OAUTH_TOKEN are set. The token is passed only
// when provider is claude-code and the binary's sha256 equals pinnedSHA;
// any other launch is refused. The returned names are safe to log.
func ClaudeEnv(provider, binary, pinnedSHA string, base []string, home string, token Secret) (env, names []string, err error) {
	if provider != "claude-code" || pinnedSHA == "" || token.kind != KindToken {
		return nil, nil, ErrTokenLaunch
	}
	got, err := FileSHA256(binary)
	if err != nil || got != pinnedSHA {
		return nil, nil, ErrTokenLaunch
	}
	if err := CheckTokenLength(len(token.value)); err != nil {
		return nil, nil, err
	}
	if home == "" || !strings.HasPrefix(home, "/") {
		return nil, nil, errors.New("credential: HOME must be an absolute per-run directory")
	}
	for _, p := range base {
		name, _, _ := strings.Cut(p, "=")
		if claudeStripped[name] || name == "HOME" {
			continue
		}
		env = append(env, p)
	}
	env = append(env, ClaudeFixedEnv...)
	env = append(env, "HOME="+home, EnvOAuthToken+"="+string(token.value))
	for _, p := range env {
		n, _, _ := strings.Cut(p, "=")
		names = append(names, n)
	}
	return env, names, nil
}

// CheckManifest is the AC-14 / NEG-11 guard over any launch's environment:
// ANTHROPIC_API_KEY is never passed, and CLAUDE_CODE_OAUTH_TOKEN only on a
// claude-code launch whose binary matches pinnedSHA. Errors name variables,
// never values.
func CheckManifest(provider, binary, pinnedSHA string, env []string) error {
	hasToken := false
	for _, p := range env {
		name, _, _ := strings.Cut(p, "=")
		switch name {
		case EnvAPIKey:
			return fmt.Errorf("credential: launch refused: %s is never passed", EnvAPIKey)
		case EnvOAuthToken:
			hasToken = true
		}
	}
	if !hasToken {
		return nil
	}
	if provider != "claude-code" || pinnedSHA == "" {
		return ErrTokenLaunch
	}
	if got, err := FileSHA256(binary); err != nil || got != pinnedSHA {
		return ErrTokenLaunch
	}
	return nil
}

// OpenStream opens a raw stream file for the worker's inherited descriptor
// (AC-15): O_WRONLY|O_APPEND|O_CREAT|O_EXCL, never following a symlink,
// mode 0600.
func OpenStream(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
