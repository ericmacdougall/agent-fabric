package credential

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Lock is the exclusive rotate flock on {dir}/.rotate.lock (AC-18).
type Lock struct{ fd int }

// LockRotate takes the rotate flock, blocking.
func (p Policy) LockRotate() (*Lock, error) {
	fd, err := unix.Open(p.Dir+"/"+LockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, refuse(RDir, "open rotate lock: %v", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		return nil, refuse(RDir, "flock: %v", err)
	}
	return &Lock{fd: fd}, nil
}

// Unlock releases the flock.
func (l *Lock) Unlock() {
	if l != nil && l.fd >= 0 {
		unix.Flock(l.fd, unix.LOCK_UN)
		unix.Close(l.fd)
		l.fd = -1
	}
}

// install writes value as claude-oauth.v{n} and makes claude-oauth a hard
// link to it by the rotation rename (AC-17, AC-18 steps 1-4). The caller
// holds the lock.
func (p Policy) install(lk *Lock, value []byte, capture bool) (int, error) {
	if lk == nil || lk.fd < 0 {
		return -1, errors.New("credential: install needs the rotate lock")
	}
	if err := CheckTokenValue(value); err != nil {
		return -1, err
	}
	h, err := p.openDir()
	if err != nil {
		return -1, err
	}
	defer h.close()
	if err := unix.Unlinkat(h.fd, TmpName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return -1, refuse(RDir, "remove stale %s: %v", TmpName, err)
	}
	vs := h.versions()
	_, hasCur := h.entries[CurrentName]
	n := 0
	if capture {
		if len(vs) > 0 || hasCur {
			return -1, refuse(RCaptureExist, "the secret directory already holds a token; use rotate")
		}
	} else {
		if !hasCur {
			return -1, refuse(RMissing, "no current token to rotate; use capture")
		}
		if len(vs) > 0 {
			n = vs[len(vs)-1] + 1
		}
	}
	name := VersionName(n)
	fd, err := unix.Openat(h.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, tokenMode)
	if err != nil {
		return -1, refuse(RDir, "create %s: %v", name, err)
	}
	fail := func(e error) (int, error) {
		unix.Close(fd)
		unix.Unlinkat(h.fd, name, 0)
		return -1, e
	}
	if p.OwnerUID >= 0 || p.GroupGID >= 0 {
		if err := unix.Fchown(fd, p.OwnerUID, p.GroupGID); err != nil {
			return fail(refuse(RDir, "chown %s: %v", name, err))
		}
	}
	if err := unix.Fchmod(fd, tokenMode); err != nil {
		return fail(refuse(RDir, "chmod %s: %v", name, err))
	}
	for off := 0; off < len(value); {
		w, err := unix.Write(fd, value[off:])
		if err != nil {
			return fail(refuse(RDir, "write %s: %v", name, err))
		}
		off += w
	}
	if err := unix.Fsync(fd); err != nil {
		return fail(refuse(RDir, "fsync %s: %v", name, err))
	}
	unix.Close(fd)
	if err := unix.Linkat(h.fd, name, h.fd, TmpName, 0); err != nil {
		unix.Unlinkat(h.fd, name, 0)
		return -1, refuse(RDir, "link %s: %v", TmpName, err)
	}
	if err := unix.Renameat(h.fd, TmpName, h.fd, CurrentName); err != nil {
		unix.Unlinkat(h.fd, TmpName, 0)
		unix.Unlinkat(h.fd, name, 0)
		return -1, refuse(RDir, "rename over %s: %v", CurrentName, err)
	}
	if err := unix.Fsync(h.fd); err != nil {
		return -1, refuse(RDir, "fsync directory: %v", err)
	}
	return n, nil
}

// Capture installs the first token as claude-oauth.v0 (AC-17) under the
// rotate lock.
func (p Policy) Capture(value []byte) (int, error) {
	lk, err := p.LockRotate()
	if err != nil {
		return -1, err
	}
	defer lk.Unlock()
	return p.install(lk, value, true)
}

// Rotate installs a new token version and runs GC (AC-18).
func (p Policy) Rotate(value []byte, bindingsDir string) (int, error) {
	lk, err := p.LockRotate()
	if err != nil {
		return -1, err
	}
	defer lk.Unlock()
	n, err := p.install(lk, value, false)
	if err != nil {
		return -1, err
	}
	if _, err := p.GCLocked(lk, bindingsDir); err != nil {
		return n, fmt.Errorf("rotated to %s; gc: %w", VersionName(n), err)
	}
	return n, nil
}

func unixUnlinkat(dirfd int, name string) error {
	if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return refuse(RDir, "remove %s: %v", name, err)
	}
	return nil
}

func unixFsync(fd int) error {
	if err := unix.Fsync(fd); err != nil {
		return refuse(RDir, "fsync directory: %v", err)
	}
	return nil
}
