package domain

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Provider is a mission's provider and an attempt's launcher path
// (goodqa#15087). A mission records it at creation and never changes it; a
// launcher states its own path when it starts an attempt, and the reducer
// and the database both refuse a path that differs from the mission's.
type Provider string

const (
	// ProviderClaudeCode: the official Claude Code binary, on the user's own
	// login. fabd launches these.
	ProviderClaudeCode Provider = "claude-code"
	// ProviderAnthropicAPI: the native loop, on an API key. Only
	// `fab mission run --grant` runs these.
	ProviderAnthropicAPI Provider = "anthropic-api"
)

// ErrInvalidProvider is a provider or path outside the two above.
var ErrInvalidProvider = errors.New("invalid provider")

// ParseProvider accepts exactly claude-code or anthropic-api.
func ParseProvider(s string) (Provider, error) {
	switch p := Provider(s); p {
	case ProviderClaudeCode, ProviderAnthropicAPI:
		return p, nil
	}
	return "", fmt.Errorf("%w: %q (want claude-code or anthropic-api)", ErrInvalidProvider, s)
}

var dotSegment = regexp.MustCompile(`(^|/)\.{1,2}(/|$)`)

// ValidLockPath mirrors the attempts_lock_path_check constraint of 0026: an
// absolute path with no "." or ".." segment and no empty segment, ending in
// /native-locks/<attempt>.lock, so it can only name this attempt's own file.
func ValidLockPath(attempt AttemptID, p string) error {
	suffix := "/native-locks/" + string(attempt) + ".lock"
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: lock path %q is not absolute", ErrInvalidID, p)
	case dotSegment.MatchString(p), strings.Contains(p, "//"):
		return fmt.Errorf("%w: lock path %q is not clean", ErrInvalidID, p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("%w: lock path %q is not clean", ErrInvalidID, p)
	case !strings.HasSuffix(p, suffix):
		return fmt.Errorf("%w: lock path %q does not end in %s", ErrInvalidID, p, suffix)
	}
	return nil
}

// MaxWorktreeBytes bounds a recorded worktree path (PATH_MAX on Linux).
const MaxWorktreeBytes = 4096

// ValidWorktree mirrors the attempts_worktree_check constraint of 0038
// (goodqa#15283): an absolute path other than "/", with no "." or ".."
// segment, no empty segment, no trailing "/", no NUL or control character,
// at most MaxWorktreeBytes bytes. It also refuses what the database leaves to
// its encoding or does not check: invalid UTF-8, and Unicode format (Cf) and
// bidi control characters, which can make two different paths look alike. It
// never folds case: the binding compares bytes.
func ValidWorktree(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: worktree is empty", ErrInvalidID)
	case len(p) > MaxWorktreeBytes:
		return fmt.Errorf("%w: worktree is longer than %d bytes", ErrInvalidID, MaxWorktreeBytes)
	case !utf8.ValidString(p):
		return fmt.Errorf("%w: worktree %q is not valid UTF-8", ErrInvalidID, p)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: worktree %q is not absolute", ErrInvalidID, p)
	case p == "/":
		return fmt.Errorf("%w: worktree may not be /", ErrInvalidID)
	case strings.HasSuffix(p, "/"), dotSegment.MatchString(p), strings.Contains(p, "//"), filepath.Clean(p) != p:
		return fmt.Errorf("%w: worktree %q is not clean", ErrInvalidID, p)
	}
	for _, r := range p {
		switch {
		case r == 0 || unicode.IsControl(r):
			return fmt.Errorf("%w: worktree %q holds a control character", ErrInvalidID, p)
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Bidi_Control, r):
			return fmt.Errorf("%w: worktree %q holds a format or bidi character U+%04X", ErrInvalidID, p, r)
		}
	}
	return nil
}
