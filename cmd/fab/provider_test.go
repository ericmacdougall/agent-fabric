package main

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15087 AC-1, NEG-1: `fab mission new` needs a valid --provider,
// and refuses before it opens a store (exit 3).
func TestMissionNewProvider(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://refused.invalid/none") // never reached
	for _, extra := range [][]string{nil, {"--provider", ""}, {"--provider", "claude.ai"}, {"--provider", "Claude-Code"}} {
		args := append([]string{"new", "--id", "m_x", "--ticket", "GoodQA goodqa#15087"}, extra...)
		err := missionMain(args)
		if code := exitCode(err); code != 3 || !errors.Is(err, domain.ErrInvalidProvider) {
			t.Errorf("mission new %v: exit %d (%v), want 3 and ErrInvalidProvider", extra, code, err)
		}
	}
}

// goodqa#15087 AC-2: `fab attempt start` needs --path, and --path
// anthropic-api needs this attempt's own --lock-path (exit 3, no store).
// The database half (anthropic-api on a claude-code mission is a stored
// refusal, exit 3) is internal/store TestStartAttemptWrongPath.
func TestAttemptStartPath(t *testing.T) {
	t.Setenv("FABRIC_DSN", "postgres://refused.invalid/none") // never reached
	base := []string{"start", "--mission", "m_x", "--id", "r_x", "--key", "k"}
	for _, c := range []struct {
		what  string
		extra []string
	}{
		{"no --path", nil},
		{"unknown --path", []string{"--path", "claude.ai"}},
		{"anthropic-api without --lock-path", []string{"--path", "anthropic-api"}},
		{"anthropic-api with another attempt's lock", []string{"--path", "anthropic-api", "--lock-path", "/s/native-locks/r_y.lock"}},
		{"anthropic-api with a relative lock", []string{"--path", "anthropic-api", "--lock-path", "s/native-locks/r_x.lock"}},
		{"claude-code with a lock path", []string{"--path", "claude-code", "--lock-path", "/s/native-locks/r_x.lock"}},
	} {
		err := attemptMain(append(append([]string{}, base...), c.extra...))
		if code := exitCode(err); code != 3 {
			t.Errorf("attempt start, %s: exit %d (%v), want 3", c.what, code, err)
		}
	}
}
