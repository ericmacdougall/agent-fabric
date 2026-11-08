package tools

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrWorktreeMoved: the attempt's recorded worktree no longer resolves to the
// directory it named when the run started (goodqa#15283). A parent directory
// was renamed or swapped, the path became a symlink, or the directory was
// replaced. The tool is refused; nothing is started.
var ErrWorktreeMoved = errors.New("worktree moved")

// WorktreePin is the one check every tool entry point runs before it starts
// (goodqa#15283 AC-5): the in-process read, grep, every confined child
// (shell and grep through HostConfiner) and every oracle hook exec. Path is
// the canonical worktree, recorded with its (dev, ino) once per run.
type WorktreePin struct {
	path string
	dev  uint64
	ino  uint64
}

// PinWorktree records path's identity. path must already be canonical: it
// must resolve to itself and be a directory.
func PinWorktree(path string) (*WorktreePin, error) {
	p := &WorktreePin{path: path}
	st, err := p.stat()
	if err != nil {
		return nil, err
	}
	p.dev, p.ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is int32 on darwin
	return p, nil
}

// Path is the canonical worktree.
func (p *WorktreePin) Path() string { return p.path }

func (p *WorktreePin) stat() (unix.Stat_t, error) {
	var st unix.Stat_t
	real, err := filepath.EvalSymlinks(p.path)
	if err != nil {
		return st, fmt.Errorf("%w: %s no longer resolves: %v", ErrWorktreeMoved, p.path, err)
	}
	if real != p.path {
		return st, fmt.Errorf("%w: %s now resolves to %s", ErrWorktreeMoved, p.path, real)
	}
	if err := unix.Lstat(p.path, &st); err != nil {
		return st, fmt.Errorf("%w: %s: %v", ErrWorktreeMoved, p.path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return st, fmt.Errorf("%w: %s is not a directory", ErrWorktreeMoved, p.path)
	}
	return st, nil
}

// Check refuses with ErrWorktreeMoved unless the canonical path still
// resolves to itself and is the directory recorded at start. A small window
// between this check and the exec that follows remains (documented residual).
func (p *WorktreePin) Check() error {
	if p == nil {
		return fmt.Errorf("%w: no worktree recorded for this run", ErrWorktreeMoved)
	}
	st, err := p.stat()
	if err != nil {
		return err
	}
	return p.same(st)
}

// CheckFD is Check for a directory already opened from Path: the descriptor
// itself must be the recorded directory, which closes the window for the
// in-process read.
func (p *WorktreePin) CheckFD(fd int) error {
	if p == nil {
		return fmt.Errorf("%w: no worktree recorded for this run", ErrWorktreeMoved)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrWorktreeMoved, p.path, err)
	}
	return p.same(st)
}

func (p *WorktreePin) same(st unix.Stat_t) error {
	if uint64(st.Dev) != p.dev || st.Ino != p.ino { //nolint:unconvert // Dev is int32 on darwin
		return fmt.Errorf("%w: %s is now another directory (dev %d ino %d, recorded dev %d ino %d)",
			ErrWorktreeMoved, p.path, uint64(st.Dev), st.Ino, p.dev, p.ino) //nolint:unconvert
	}
	return nil
}
