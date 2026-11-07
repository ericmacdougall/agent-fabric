package native

import (
	"fmt"
	"os"
	"path/filepath"

	"fabric/internal/domain"
)

// StateDir is where the native runner keeps host-local state:
// FABRIC_STATE_DIR, else ~/.local/state/fabric. It is resolved once, by the
// runner, when it starts an attempt; the prober never recomputes it.
func StateDir() (string, error) {
	if d := os.Getenv("FABRIC_STATE_DIR"); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("native state dir: %w", err)
	}
	return filepath.Abs(filepath.Join(home, ".local", "state", "fabric"))
}

// LockPath is the attempt's liveness lock file (goodqa#15087), recorded in
// attempts.lock_path when the attempt starts.
func LockPath(attempt domain.AttemptID) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "native-locks", string(attempt)+".lock")
	if err := domain.ValidLockPath(attempt, p); err != nil {
		return "", err
	}
	return p, nil
}
