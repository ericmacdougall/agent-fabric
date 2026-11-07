package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"fabric/internal/agent"
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

// acquireLock takes the attempt's exclusive, non-blocking flock and returns
// the open file; the flock lasts until the file is closed or the process
// dies, whatever happens to the file. Another holder is ErrAttemptBusy.
// The file is opened O_NOFOLLOW (a symlink at the path is refused) and
// created 0600 in a 0700 directory (goodqa#15087 V3).
func acquireLock(attempt domain.AttemptID, path string) (*os.File, error) {
	if err := domain.ValidLockPath(attempt, path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("native lock dir: %w", err)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("native lock %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var stt unix.Stat_t
	if err := unix.Fstat(fd, &stt); err != nil || stt.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("native lock %s is not a regular file", path)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: attempt busy: %s (another process holds %s)", agent.ErrAttemptBusy, attempt, path)
		}
		return nil, fmt.Errorf("native lock %s: %w", path, err)
	}
	return f, nil
}

// ProbeLock reports whether a live process holds the attempt's flock
// (goodqa#15087 AC-7). It reads the path from the attempt row, never from
// this process's environment. Anything that is not a regular file at
// exactly this attempt's lock path held by another process reads as gone:
// a NULL path, a missing file, a symlink, a FIFO, a file it cannot open or
// lock, and a file whose last holder died. It never blocks.
func ProbeLock(attempt domain.AttemptID, path string) (alive bool) {
	if path == "" || domain.ValidLockPath(attempt, path) != nil {
		return false
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var stt unix.Stat_t
	if err := unix.Fstat(fd, &stt); err != nil || stt.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		unix.Flock(fd, unix.LOCK_UN) //nolint:errcheck // closing releases it anyway
		return false
	}
	return errors.Is(err, unix.EWOULDBLOCK)
}
