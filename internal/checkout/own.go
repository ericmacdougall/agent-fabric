// Package checkout builds the per-attempt worktree of an M1 mission
// (goodqa#15966 AC-23): a fresh directory extracted with `git archive` from
// an operator-owned bare mirror, read-only, and owned by nobody the worker
// can act as. It also holds the ownership rule the mission config loader
// (internal/mission) applies to its own files.
package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrOwnership refuses a path that the worker account could change.
var ErrOwnership = errors.New("ownership refused")

// Info is the part of a stat result the ownership rule reads.
type Info struct {
	UID, GID uint32
	Mode     os.FileMode // includes the type bits
}

// Stater is the stat seam. Production uses OS; tests, which do not run as
// root, substitute owners and modes for chosen paths.
type Stater interface {
	Lstat(path string) (Info, error)
	Fstat(f *os.File) (Info, error)
}

// OS is the production Stater.
type OS struct{}

func fromFileInfo(fi os.FileInfo) (Info, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, fmt.Errorf("%w: no owner in stat result", ErrOwnership)
	}
	return Info{UID: st.Uid, GID: st.Gid, Mode: fi.Mode()}, nil
}

func (OS) Lstat(path string) (Info, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Info{}, err
	}
	return fromFileInfo(fi)
}

func (OS) Fstat(f *os.File) (Info, error) {
	fi, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	return fromFileInfo(fi)
}

// Principals names the accounts the rule distinguishes: the operator (the
// uid fabd runs as) and the worker account with every group it belongs to.
type Principals struct {
	OperatorUID uint32
	WorkerUID   uint32
	WorkerGIDs  []uint32
}

// Validate refuses principals that cannot separate operator from worker.
func (p Principals) Validate() error {
	if p.WorkerUID == p.OperatorUID {
		return fmt.Errorf("%w: the worker uid equals the operator uid", ErrOwnership)
	}
	if p.WorkerUID == 0 {
		return fmt.Errorf("%w: the worker uid is root", ErrOwnership)
	}
	return nil
}

// WorkerInGroup reports whether gid is one of the worker's groups.
func (p Principals) WorkerInGroup(gid uint32) bool {
	for _, g := range p.WorkerGIDs {
		if g == gid {
			return true
		}
	}
	return false
}

// WritableByWorker reports whether the worker could write i through its
// permission bits: as owner, through a group it is in, or as other. A sticky
// directory still counts: AC-23 says no ancestor is writable by fab-worker.
func (p Principals) WritableByWorker(i Info) bool {
	perm := i.Mode.Perm()
	switch {
	case i.UID == p.WorkerUID:
		return true // the owner can chmod it whatever its bits say
	case perm&0o002 != 0:
		return true
	case perm&0o020 != 0 && p.WorkerInGroup(i.GID):
		return true
	}
	return false
}

// ReadableByWorker reports whether the worker can read i.
func (p Principals) ReadableByWorker(i Info) bool {
	perm := i.Mode.Perm()
	switch {
	case i.UID == p.WorkerUID:
		return perm&0o400 != 0
	case p.WorkerInGroup(i.GID):
		return perm&0o040 != 0
	}
	return perm&0o004 != 0
}

// CheckOwned refuses i (named label in errors) unless the operator or root
// owns it and the worker cannot write it.
func (p Principals) CheckOwned(i Info, label string) error {
	if i.UID != p.OperatorUID && i.UID != 0 {
		return fmt.Errorf("%w: %s is owned by uid %d, not the operator or root", ErrOwnership, label, i.UID)
	}
	if p.WritableByWorker(i) {
		return fmt.Errorf("%w: %s is writable by the worker account (%v)", ErrOwnership, label, i.Mode.Perm())
	}
	return nil
}

// CheckAncestors refuses unless dir and every directory above it up to "/"
// is a real directory (not a symlink), owned by the operator or root, and
// not writable by the worker. dir must be absolute and clean; callers that
// accept a symlinked path resolve it first.
func CheckAncestors(st Stater, p Principals, dir, label string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("%w: %s: %q is not an absolute clean path", ErrOwnership, label, dir)
	}
	for cur := dir; ; {
		i, err := st.Lstat(cur)
		if err != nil {
			return fmt.Errorf("%w: %s: %s cannot be checked: %v", ErrOwnership, label, cur, err)
		}
		if i.Mode&os.ModeSymlink != 0 || !i.Mode.IsDir() {
			return fmt.Errorf("%w: %s: %s is not a directory", ErrOwnership, label, cur)
		}
		if err := p.CheckOwned(i, label+": directory "+cur); err != nil {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}
