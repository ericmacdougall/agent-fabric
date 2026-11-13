package tools

// Shared machinery of the edit and write tools (goodqa#15361 N1). Every path
// step is opened relative to the worktree descriptor with O_NOFOLLOW; the
// root descriptor is verified by WorktreePin.CheckFD; the target is replaced
// by an atomic renameat of a temp file in the same directory.
//
// Designs ported from earendil-works/pi (MIT, see NOTICE): the exact-first,
// normalised-fallback edit matching and the atomic replace.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/sys/unix"

	"fabric/internal/agent"
)

const (
	// MaxFileBytes bounds the file an edit reads and the result either tool writes.
	MaxFileBytes = 1 << 20
	// MaxDiffBytes caps the unified diff in a tool result.
	MaxDiffBytes = 8 << 10
	maxEdits     = 64
)

var (
	errNoDir   = errors.New("no such directory")
	errChanged = errors.New("file changed since read")
	// ErrNeedsReview is not used here; see reviewgate wiring in internal/native.
)

// fsCore is what Edit and Write share.
type fsCore struct {
	read *Read // path rules and root
	// Pin is the run's recorded worktree; required.
	Pin *WorktreePin
	// Preimage keeps a copy of a target git cannot restore (untracked,
	// ignored, dirty, staged-only) before it is replaced. Nil disables the
	// copy; the native toolset never builds edit or write without one.
	Preimage *PreimageKeeper
}

// hooks for tests (race and crash windows). Nil in production.
var (
	testAfterLoad    func()
	testBeforeRename func()
	testAfterRename  func()
)

type snapshot struct {
	exists bool
	ino    uint64
	mode   uint32
	data   []byte
	sha    string
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// gitName reports whether a path component names the git directory the way
// git's verify_path would: case-folded, ignoring the HFS ignorable code
// points, and with NTFS trailing dot/space and the 8.3 short name folded.
func gitName(c string) bool {
	var b strings.Builder
	for _, r := range c {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := strings.TrimRight(b.String(), ". ")
	return s == ".git" || s == "git~1"
}

// resolve turns the model's path into components and refuses .git.
func (f *fsCore) resolve(p string) ([]string, error) {
	parts, err := f.read.rel(p)
	if err != nil {
		return nil, err
	}
	for _, c := range parts {
		if gitName(c) {
			return nil, errors.New("refusing to touch .git")
		}
	}
	return parts, nil
}

// openRoot opens the worktree and verifies the descriptor is the pinned one.
func (f *fsCore) openRoot() (int, error) {
	if err := f.Pin.Check(); err != nil {
		return -1, err
	}
	fd, err := unix.Open(f.read.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("worktree: %w", err)
	}
	if err := f.Pin.CheckFD(fd); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

type madeDir struct {
	parent int // owned descriptor of the parent
	name   string
}

// parentDir walks parts[:n-1] with openat(O_NOFOLLOW|O_DIRECTORY). With create
// it makes missing directories (mkdirat 0755) and returns them so a failed
// write can remove them. The caller closes the returned descriptor and every
// madeDir.parent through release.
func (f *fsCore) parentDir(parts []string, create bool) (fd int, made []madeDir, err error) {
	cur, err := f.openRoot()
	if err != nil {
		return -1, nil, err
	}
	fail := func(e error) (int, []madeDir, error) {
		unix.Close(cur)
		releaseDirs(made, false)
		return -1, nil, e
	}
	for _, c := range parts[:len(parts)-1] {
		next, oerr := unix.Openat(cur, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(oerr, unix.ENOENT) && create {
			if merr := unix.Mkdirat(cur, c, 0o755); merr != nil && !errors.Is(merr, unix.EEXIST) {
				return fail(fmt.Errorf("mkdir: %w", merr))
			}
			dup, derr := unix.Dup(cur)
			if derr != nil {
				return fail(derr)
			}
			made = append(made, madeDir{parent: dup, name: c})
			next, oerr = unix.Openat(cur, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if oerr != nil {
			switch {
			case errors.Is(oerr, unix.ELOOP), errors.Is(oerr, unix.EMLINK), errors.Is(oerr, unix.ENOTDIR):
				return fail(errors.New("symbolic links are not followed and every parent must be a directory"))
			case errors.Is(oerr, unix.ENOENT):
				return fail(errNoDir)
			}
			return fail(fmt.Errorf("open: %w", oerr))
		}
		unix.Close(cur)
		cur = next
	}
	return cur, made, nil
}

// releaseDirs closes the made-directory descriptors; with rollback it first
// removes the directories this call created, deepest first, when empty.
func releaseDirs(made []madeDir, rollback bool) {
	for i := len(made) - 1; i >= 0; i-- {
		if rollback {
			_ = unix.Unlinkat(made[i].parent, made[i].name, unix.AT_REMOVEDIR)
		}
		unix.Close(made[i].parent)
	}
}

// load reads the target relative to the parent descriptor. A missing file is
// {exists:false}. A final symlink, a non-regular file, a hard-linked file and
// one over the size limit are refused.
func loadTarget(parent int, name string) (snapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return snapshot{}, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return snapshot{}, errors.New("symbolic links are not followed")
		}
		return snapshot{}, fmt.Errorf("open: %w", err)
	}
	f := os.NewFile(uintptr(fd), "target")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return snapshot{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return snapshot{}, errors.New("not a regular file")
	}
	if st.Nlink > 1 {
		return snapshot{}, errors.New("file has more than one hard link")
	}
	if st.Size > MaxFileBytes {
		return snapshot{}, fmt.Errorf("file is larger than %d bytes", MaxFileBytes)
	}
	buf := make([]byte, 0, st.Size+1)
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > MaxFileBytes {
			return snapshot{}, fmt.Errorf("file is larger than %d bytes", MaxFileBytes)
		}
		if rerr != nil {
			break
		}
	}
	return snapshot{exists: true, ino: st.Ino, mode: uint32(st.Mode & 0o7777), data: buf, sha: sha256hex(buf)}, nil
}

type lockKey struct {
	dev, ino uint64
	name     string
}

var fileLocks sync.Map // lockKey -> *sync.Mutex

func lockFor(parent int, name string) (*sync.Mutex, error) {
	var st unix.Stat_t
	if err := unix.Fstat(parent, &st); err != nil {
		return nil, err
	}
	m, _ := fileLocks.LoadOrStore(lockKey{uint64(st.Dev), st.Ino, name}, new(sync.Mutex)) //nolint:unconvert // Dev is int32 on darwin
	return m.(*sync.Mutex), nil
}

// tagOf is the 8-character, filename-safe tail of a tool call id.
func tagOf(callID string) string {
	var b strings.Builder
	for _, r := range callID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	id := b.String()
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return id
}

// callTag is the tag of the loop-attached call, or 8 random hex characters
// for a direct call that has none.
func callTag(ctx context.Context) string {
	if call, ok := agent.ToolCallFromContext(ctx); ok {
		if t := tagOf(call.ID); t != "" {
			return t
		}
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

// tempName is the recognisable per-call temp file: .fabric-<callid8>.tmp.
func tempName(tag string) string { return ".fabric-" + tag + ".tmp" }

// commit replaces name in parent with data, atomically. pre is the pre-image
// the result was computed from; the replace is refused when the file is no
// longer exactly that (lost-update guard, FN-3). It returns the post sha.
func (f *fsCore) commit(tag, rel string, parent int, name string, pre snapshot, data []byte) error {
	mu, err := lockFor(parent, name)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	cur, err := loadTarget(parent, name)
	if err != nil {
		return err
	}
	if cur.exists != pre.exists || cur.sha != pre.sha || (pre.exists && cur.ino != pre.ino) {
		return errChanged
	}
	mode := uint32(0o644)
	if pre.exists {
		mode = pre.mode
	}
	tmp := tempName(tag)
	_ = unix.Unlinkat(parent, tmp, 0) // a leftover of this same call after a crash
	fd, err := unix.Openat(parent, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	cleanup := func() { _ = unix.Unlinkat(parent, tmp, 0) }
	file := os.NewFile(uintptr(fd), tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		cleanup()
		return fmt.Errorf("write: %w", err)
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		file.Close()
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	file.Close()
	f.Preimage.writeJournal(tag, rel, pre.sha, sha256hex(data))
	if testBeforeRename != nil {
		testBeforeRename()
	}
	again, err := loadTarget(parent, name)
	if err != nil {
		cleanup()
		return err
	}
	if again.exists != pre.exists || again.sha != pre.sha || (pre.exists && again.ino != pre.ino) {
		cleanup()
		return errChanged
	}
	if err := unix.Renameat(parent, tmp, parent, name); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	if testAfterRename != nil {
		testAfterRename()
	}
	_ = unix.Fsync(parent)
	return nil
}

// finish re-checks the pin after the rename: a moved worktree is reported,
// never hidden.
func (f *fsCore) finish() error { return f.Pin.Check() }

// recoverText is what the loop shows for a side-effecting call it found
// EXECUTING on resume: it removes this call's temp file and tells the model
// the file's recorded pre-image digest, the intended post-image digest and
// the file's current digest, so it can tell whether the write landed.
func (f *fsCore) recoverText(callID, path string) string {
	parts, err := f.resolve(path)
	if err != nil {
		return ""
	}
	parent, made, err := f.parentDir(parts, false)
	if err != nil {
		return ""
	}
	defer func() { unix.Close(parent); releaseDirs(made, false) }()
	tag := tagOf(callID)
	_ = unix.Unlinkat(parent, tempName(tag), 0)
	cur, err := loadTarget(parent, parts[len(parts)-1])
	if err != nil {
		return ""
	}
	state := "missing"
	if cur.exists {
		state = cur.sha
	}
	text := fmt.Sprintf(" File %s currently has sha256 %s.", path, state)
	if j, ok := f.Preimage.readJournal(tag); ok && j.Path == path {
		text += fmt.Sprintf(" Recorded pre-image sha256 %s, intended post-image sha256 %s.", j.Pre, j.Post)
	}
	return text
}

func lineCount(b []byte) int { return bytes.Count(b, []byte{'\n'}) }
