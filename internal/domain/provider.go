package domain

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
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
