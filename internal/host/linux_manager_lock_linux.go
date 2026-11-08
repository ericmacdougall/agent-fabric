//go:build linux

package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockRuntimeBase is the controller's systemd runtime directory, derived
// from its uid and never from the environment (goodqa#15235 S5b red-team L2):
// a daemon started without XDG_RUNTIME_DIR must lock the same inode as one
// started as a user service. It persists across logouts only with linger,
// which the Fabric controller account has. One controller account per worker
// uid is supported; two controller accounts sharing a worker are not.
var lockRuntimeBase = func() string { return "/run/user/" + strconv.Itoa(os.Getuid()) }

// workerManagerLockPath returns a controller-owned path, never a predictable
// inode directly in a shared temporary directory. Dedicated workers cannot
// traverse or replace the lock when the controller directory remains 0700.
func workerManagerLockPath(configured string, workerUID int) (string, error) {
	path := configured
	if path == "" {
		base := lockRuntimeBase()
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			return "", fmt.Errorf("worker-manager lock base %s is unavailable (controller needs a systemd runtime directory with linger): %v", base, err)
		}
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("worker-manager lock base %q is not absolute", base)
		}
		dir := filepath.Join(base, "fab", "locks")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create worker-manager lock directory: %w", err)
		}
		path = filepath.Join(dir, "worker-manager-"+strconv.Itoa(workerUID)+".lock")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("worker-manager lock path %q is not absolute", path)
	}
	dir := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve worker-manager lock directory: %w", err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat worker-manager lock directory: %w", err)
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || int(owner.Uid) != os.Getuid() || st.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("worker-manager lock directory %s must be controller-owned mode 0700", resolved)
	}
	if err := validateLockDirectoryChain(resolved); err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// validateLockDirectoryChain prevents a worker from renaming the private lock
// directory through a writable ancestor. Root-owned sticky directories such as
// /tmp are safe; every other ancestor must be root/controller-owned and not
// group/world writable.
func validateLockDirectoryChain(path string) error {
	for dir := path; ; dir = filepath.Dir(dir) {
		st, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat worker-manager lock ancestor: %w", err)
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok || !st.IsDir() {
			return fmt.Errorf("worker-manager lock ancestor %s is not a directory", dir)
		}
		uid := int(owner.Uid)
		if uid != os.Getuid() && uid != 0 {
			return fmt.Errorf("worker-manager lock ancestor %s has untrusted owner %d", dir, uid)
		}
		stickyRoot := uid == 0 && st.Mode()&os.ModeSticky != 0
		if st.Mode().Perm()&0o022 != 0 && !stickyRoot {
			return fmt.Errorf("worker-manager lock ancestor %s is writable by group or others", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
	}
}

func lockInodeMatches(path string, expected os.FileInfo) error {
	actual, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("worker-manager lock inode: %w", err)
	}
	owner, ok := actual.Sys().(*syscall.Stat_t)
	if !ok || !actual.Mode().IsRegular() || actual.Mode().Perm() != 0o600 || owner.Nlink != 1 || int(owner.Uid) != os.Getuid() {
		return fmt.Errorf("worker-manager lock %s is not a singly-linked controller-owned mode-0600 regular file", path)
	}
	if expected != nil && !os.SameFile(expected, actual) {
		return fmt.Errorf("worker-manager lock %s was replaced while held", path)
	}
	return nil
}

// lockWorkerManager serializes cooperating daemons for one worker manager.
// O_NOFOLLOW, directory ownership/mode and inode revalidation prevent a worker
// from redirecting or replacing the flock target.
func (l *LinuxManager) lockWorkerManager(uid int) error {
	path, err := workerManagerLockPath(l.LockPath, uid)
	if err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open worker-manager lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open worker-manager lock returned no file")
	}
	fail := func(err error) error {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat worker-manager lock: %w", err))
	}
	if err := lockInodeMatches(path, nil); err != nil {
		return fail(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("%w: worker manager flock: %v", ErrWorkerManagerBusy, err)
		}
		return fmt.Errorf("worker manager flock: %w", err)
	}
	if err := lockInodeMatches(path, info); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return fail(err)
	}
	l.runLock = file
	l.runLockInfo = info
	return nil
}

// releaseRun opens the in-process gate only after the cross-process lock is
// released successfully. Any incomplete cleanup leaves the profile refused.
func (l *LinuxManager) releaseRun() error {
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	return l.releaseRunLocked()
}

func (l *LinuxManager) releaseRunLocked() error {
	if l.runLock != nil {
		file := l.runLock
		if err := lockInodeMatches(file.Name(), l.runLockInfo); err != nil {
			return fmt.Errorf("refuse worker-manager unlock: %w", err)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
			return fmt.Errorf("unlock worker manager: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close worker-manager lock: %w", err)
		}
		l.runLock = nil
		l.runLockInfo = nil
	}
	l.mu.Lock()
	l.activeSerial = 0
	l.active = Handle{}
	l.mu.Unlock()
	l.runGate.unlock()
	return nil
}

// assertNoAbandonedRuns refuses a reused manager after another daemon crashed
// or exited without verified Stop. The OS may release its flock; leftover units
// are the durable fail-closed evidence that cleanup is still incomplete.
func assertNoAbandonedRuns(uid int) error {
	return assertNoAbandonedRunsAt("/proc", uid)
}

func assertNoAbandonedRunsAt(root string, uid int) error {
	managerPrefix := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/fab-", uid, uid)
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("scan abandoned worker units: %w", err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		owner, exists, err := procUIDAt(root, pid)
		if err != nil {
			return fmt.Errorf("scan abandoned worker units: %w", err)
		}
		if !exists || owner != uid {
			continue
		}
		cgroup, exists, err := readProcFile(fmt.Sprintf("%s/%d/cgroup", root, pid))
		if err != nil {
			return fmt.Errorf("scan abandoned worker units: %w", err)
		}
		if !exists {
			continue
		}
		text := strings.TrimSpace(string(cgroup))
		if !strings.HasPrefix(text, "0::/") {
			return fmt.Errorf("scan abandoned worker units: pid %d has malformed cgroup record", pid)
		}
		path := strings.TrimPrefix(text, "0::")
		if strings.HasPrefix(path, managerPrefix) {
			return fmt.Errorf("abandoned worker unit %s (pid %d); verified cleanup required before another Start", path, pid)
		}
	}
	return nil
}
