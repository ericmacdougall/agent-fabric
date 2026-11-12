package tools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// RunnerDir is a directory only the runner owns (goodqa#15361 FN-1): mode
// 0700, our uid, held open as a descriptor with O_DIRECTORY|O_NOFOLLOW.
// Every create, open and evict is relative to that descriptor with
// O_NOFOLLOW, and a file is read only when it is regular with one link. It is
// never placed under RunDir, which a confined child can write.
type RunnerDir struct {
	path     string
	fd       int
	maxFiles int
	maxBytes int64
	// keepAll (OpenKeepDir): nothing is ever evicted; CreateReserved refuses
	// at the limit instead. Only names starting with count are counted.
	keepAll bool
	count   string

	mu    sync.Mutex
	files []runnerFile // oldest first
}

type runnerFile struct {
	name string
	size int64
}

// ErrExpired is returned for a name that is not (or no longer) in the directory.
var ErrExpired = errors.New("expired")

// ErrPreimageStoreFull is returned by CreateReserved on a keep directory at
// its file or byte limit. Nothing is evicted; the write that wanted the copy
// is refused (goodqa#15361 F2).
var ErrPreimageStoreFull = errors.New("pre-image store is full")

// OpenRunnerDir creates (0700) and holds dir. The parent chain is created
// with MkdirAll; dir itself must be owned by this uid with mode 0700, which
// is also verified for an existing directory (a resumed attempt).
func OpenRunnerDir(dir string, maxFiles int, maxBytes int64) (*RunnerDir, error) {
	return openRunnerDir(dir, maxFiles, maxBytes, false, "")
}

// OpenKeepDir is OpenRunnerDir for copies that must never be evicted (the
// pre-image store). Only files whose name starts with count (for example
// "pre-") count toward maxFiles and maxBytes, so call journals and records in
// the same directory never consume the limit. At the limit CreateReserved
// returns ErrPreimageStoreFull.
func OpenKeepDir(dir, count string, maxFiles int, maxBytes int64) (*RunnerDir, error) {
	if count == "" {
		return nil, errors.New("keep dir needs a counted prefix")
	}
	return openRunnerDir(dir, maxFiles, maxBytes, true, count)
}

func openRunnerDir(dir string, maxFiles int, maxBytes int64, keepAll bool, count string) (*RunnerDir, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("runner dir must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if err := unix.Mkdir(dir, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("runner dir: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if int(st.Uid) != os.Getuid() || st.Mode&0o777 != 0o700 {
		unix.Close(fd)
		return nil, fmt.Errorf("runner dir %q must be owned by this user with mode 0700", dir)
	}
	d := &RunnerDir{path: dir, fd: fd, maxFiles: maxFiles, maxBytes: maxBytes, keepAll: keepAll, count: count}
	if err := d.scan(); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return d, nil
}

// scan rebuilds the eviction order from what a previous run left (oldest
// modification time first).
func (d *RunnerDir) scan() error {
	dup, err := unix.Dup(d.fd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), "rdir")
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		var st unix.Stat_t
		if d.keepAll && !strings.HasPrefix(e.Name(), d.count) {
			continue // journals and records: not copies, never counted
		}
		if unix.Fstatat(d.fd, e.Name(), &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		d.files = append(d.files, runnerFile{e.Name(), st.Size})
	}
	return nil
}

// Path is the directory's path (for tests and logs only; never opened by path).
func (d *RunnerDir) Path() string { return d.path }

// CreateReserved is Create for a keep directory: under the lock it checks
// that one more file of size bytes stays within both limits, creates the
// file and registers it, so concurrent callers cannot overshoot. At the limit
// it returns ErrPreimageStoreFull and removes nothing. The caller writes and
// closes; on failure it calls Abort, which releases the reservation.
func (d *RunnerDir) CreateReserved(prefix string, size int64) (*os.File, string, error) {
	if !d.keepAll || !strings.HasPrefix(prefix, d.count) {
		return nil, "", errors.New("runner dir: CreateReserved needs a keep dir and its counted prefix")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var total int64
	for _, f := range d.files {
		total += f.size
	}
	if len(d.files)+1 > d.maxFiles || total+size > d.maxBytes {
		return nil, "", fmt.Errorf("%w (%d of %d copies, %d of %d bytes, this copy %d bytes); the call was refused so that no earlier copy is removed",
			ErrPreimageStoreFull, len(d.files), d.maxFiles, total, d.maxBytes, size)
	}
	f, name, err := d.Create(prefix)
	if err != nil {
		return nil, "", err
	}
	d.files = append(d.files, runnerFile{name, size})
	return f, name, nil
}

// Create makes prefix+16 random hex characters with O_EXCL, so an existing
// file is never overwritten. The caller writes, closes, then calls Done.
func (d *RunnerDir) Create(prefix string) (*os.File, string, error) {
	for i := 0; i < 8; i++ {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return nil, "", err
		}
		name := prefix + hex.EncodeToString(r[:])
		fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", errors.New("runner dir: no free name")
}

// Done registers a created file's size and evicts the oldest files beyond the
// limits (never the one just added).
func (d *RunnerDir) Done(name string, size int64) {
	if d.keepAll {
		return // registered by CreateReserved; a keep dir never evicts
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files = append(d.files, runnerFile{name, size})
	var total int64
	for _, f := range d.files {
		total += f.size
	}
	for len(d.files) > 1 && (len(d.files) > d.maxFiles || total > d.maxBytes) {
		old := d.files[0]
		d.files = d.files[1:]
		total -= old.size
		_ = unix.Unlinkat(d.fd, old.name, 0)
	}
}

// Abort removes a file that was created but not completed, and releases its
// reservation in a keep dir.
func (d *RunnerDir) Abort(name string) {
	d.mu.Lock()
	for i, f := range d.files {
		if f.name == name {
			d.files = append(d.files[:i], d.files[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
	_ = unix.Unlinkat(d.fd, name, 0)
}

// writeRecord creates name (0600, O_EXCL, O_NOFOLLOW) holding b and syncs it.
// An existing name is an error: a record is written once.
func (d *RunnerDir) writeRecord(name string, b []byte) error {
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(b); err != nil {
		f.Close()
		_ = unix.Unlinkat(d.fd, name, 0)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = unix.Unlinkat(d.fd, name, 0)
		return err
	}
	return f.Close()
}

// readRecord reads a record written by writeRecord: a plain single-link
// file (Open's rule) owned by this uid, mode 0600, at most 4 KiB. A missing
// record is ErrExpired.
func (d *RunnerDir) readRecord(name string) ([]byte, error) {
	f, err := d.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm() != 0o600 || st.Size() > 4096 {
		return nil, errors.New("record is not a runner-written file")
	}
	b := make([]byte, st.Size()+1)
	n, err := io.ReadFull(f, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if int64(n) != st.Size() {
		return nil, errors.New("record changed while read")
	}
	return b[:n], nil
}

// Open opens a regular, single-link file for reading. Anything else (a
// symlink, a hard-linked file, a missing name) is refused; a missing name is
// ErrExpired.
func (d *RunnerDir) Open(name string) (*os.File, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return nil, errors.New("bad name")
	}
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrExpired
	}
	if err != nil {
		return nil, errors.New("not readable")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		unix.Close(fd)
		return nil, errors.New("spill file is not a plain single-link file")
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Close releases the descriptor; Remove also deletes the directory and what
// is in it.
func (d *RunnerDir) Close() { unix.Close(d.fd) }

func (d *RunnerDir) Remove() {
	d.mu.Lock()
	for _, f := range d.files {
		_ = unix.Unlinkat(d.fd, f.name, 0)
	}
	d.files = nil
	d.mu.Unlock()
	unix.Close(d.fd)
	_ = unix.Rmdir(d.path)
}

// ReapOrphanDirs removes the per-attempt directories under root that keep
// does not claim (attempts no longer running), so an abnormal exit leaves no
// orphan (goodqa#15361 R2-c). It returns the names it removed.
func ReapOrphanDirs(root string, keep func(name string) bool) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		if keep(e.Name()) {
			continue
		}
		if os.RemoveAll(filepath.Join(root, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}
