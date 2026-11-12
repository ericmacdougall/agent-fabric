// Package tools implements the native loop's tools. read runs in the
// runner process with every path step opened relative to the worktree
// descriptor (no symlink follow, no device or FIFO open, no hard-link
// escape); grep and shell run as confined children through a Confiner.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"fabric/internal/agent"
)

// Read is the read tool.
type Read struct {
	root     string
	given    string        // the path as configured, before symlink resolution
	MaxBytes int64         // bytes returned at most
	Timeout  time.Duration // wall bound for one read
	// Pin, when set, is the run's recorded worktree (goodqa#15283): every
	// read first checks it, then checks that the root it opened is that same
	// directory, and refuses with ErrWorktreeMoved otherwise.
	Pin *WorktreePin
	// Spill, when set, is the attempt's runner-only spill directory: a path
	// "spill:<16 hex>" resolves only there (goodqa#15361 N2).
	Spill *RunnerDir
}

var _ agent.Tool = (*Read)(nil)

// NewRead fixes the worktree root. The root path itself comes from the
// trusted grant and is resolved once (it may legitimately sit behind a
// symlink such as /var on macOS); everything below it is never followed.
func NewRead(worktree string, maxBytes int64, timeout time.Duration) (*Read, error) {
	real, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(real) {
		return nil, errors.New("tools: worktree must be absolute")
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Read{given: filepath.Clean(worktree), root: real, MaxBytes: maxBytes, Timeout: timeout}, nil
}

// Root is the resolved worktree root every read opens.
func (r *Read) Root() string { return r.root }

func (r *Read) Name() string        { return "read" }
func (r *Read) SideEffecting() bool { return false }
func (r *Read) Def() json.RawMessage {
	return json.RawMessage(`{"name":"read","description":"Read a regular text file inside the worktree. Path is relative to the worktree root, or spill:<id> for saved tool output. offset (1-based line) and limit (at most 2000 lines) read a window.","input_schema":{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":2000}},"required":["path"]}}`)
}

func (r *Read) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Path == "" {
		return "read needs {\"path\": \"relative/file\"}", true
	}
	win := window{}
	if in.Offset != nil || in.Limit != nil {
		win.set = true
		win.offset, win.limit = 1, 2000
		if in.Offset != nil {
			if *in.Offset < 1 {
				return "read refused: offset is 1-based", true
			}
			win.offset = *in.Offset
		}
		if in.Limit != nil {
			if *in.Limit < 1 || *in.Limit > 2000 {
				return "read refused: limit must be 1 to 2000", true
			}
			win.limit = *in.Limit
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if id, ok := strings.CutPrefix(in.Path, "spill:"); ok {
		out, err := r.readSpill(ctx, id, win)
		if err != nil {
			return "read refused: " + err.Error(), true
		}
		return out, false
	}
	out, err := r.read(ctx, in.Path, win)
	if err != nil {
		return "read refused: " + err.Error(), true
	}
	return out, false
}

// rel turns the model's path into clean components relative to the root.
// Absolute paths are accepted only when they name something under the root
// textually; ".." never survives, because it would be resolved by the kernel
// against a directory the model may have swapped.
func (r *Read) rel(p string) ([]string, error) {
	if strings.ContainsRune(p, 0) {
		return nil, errors.New("NUL in path")
	}
	if filepath.IsAbs(p) {
		var rp string
		ok := false
		for _, base := range []string{r.root, r.given} {
			x, err := filepath.Rel(base, filepath.Clean(p))
			if err == nil && x != ".." && !strings.HasPrefix(x, ".."+string(filepath.Separator)) {
				rp, ok = x, true
				break
			}
		}
		if !ok {
			return nil, errors.New("path is outside the worktree")
		}
		p = rp
	}
	var parts []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
		case "..":
			return nil, errors.New("path contains ..")
		default:
			parts = append(parts, c)
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("path names the worktree itself")
	}
	return parts, nil
}

func (r *Read) read(ctx context.Context, p string, win window) (string, error) {
	parts, err := r.rel(p)
	if err != nil {
		return "", err
	}
	if r.Pin != nil {
		if err := r.Pin.Check(); err != nil {
			return "", err
		}
	}
	dirfd, err := unix.Open(r.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	if r.Pin != nil {
		if err := r.Pin.CheckFD(dirfd); err != nil {
			unix.Close(dirfd)
			return "", err
		}
	}
	for i, c := range parts {
		last := i == len(parts)-1
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		if !last {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(dirfd, c, flags, 0)
		unix.Close(dirfd)
		if err != nil {
			switch {
			case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK):
				return "", errors.New("symbolic links are not followed")
			case errors.Is(err, unix.ENOENT):
				return "", errors.New("no such file")
			case errors.Is(err, unix.ENOTDIR):
				return "", errors.New("not a directory on the path")
			}
			return "", fmt.Errorf("open: %w", err)
		}
		dirfd = fd
	}
	f := os.NewFile(uintptr(dirfd), "read")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(dirfd, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", errors.New("not a regular file")
	}
	if st.Nlink > 1 {
		return "", errors.New("file has more than one hard link")
	}
	return r.stream(ctx, f, win)
}

// window is an optional 1-based line window.
type window struct {
	set           bool
	offset, limit int
}

var spillID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (r *Read) readSpill(ctx context.Context, id string, win window) (string, error) {
	if !spillID.MatchString(id) {
		return "", errors.New("a spill reference is spill: followed by exactly 16 lowercase hex characters")
	}
	if r.Spill == nil {
		return "", errors.New("no spill directory for this attempt")
	}
	f, err := r.Spill.Open(id)
	if errors.Is(err, ErrExpired) {
		return "", errors.New("spill expired")
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	return r.stream(ctx, f, win)
}

// stream returns the whole file up to MaxBytes, or, with a window, the
// requested lines up to MaxBytes, saying where to continue.
func (r *Read) stream(ctx context.Context, f *os.File, win window) (string, error) {
	if win.set {
		return r.lines(ctx, f, win)
	}
	buf := make([]byte, 0, min(r.MaxBytes, 1<<16))
	chunk := make([]byte, 32<<10)
	for int64(len(buf)) < r.MaxBytes {
		if err := ctx.Err(); err != nil {
			return "", errors.New("read timed out")
		}
		n, err := f.Read(chunk[:min(int64(len(chunk)), r.MaxBytes-int64(len(buf)))])
		buf = append(buf, chunk[:n]...)
		if errors.Is(err, io.EOF) {
			return string(buf), nil
		}
		if err != nil {
			return "", err
		}
	}
	// Hit the cap: say so when more data remains.
	var one [1]byte
	if n, _ := f.Read(one[:]); n > 0 {
		return string(buf) + "\n[truncated at " + fmt.Sprint(r.MaxBytes) + " bytes]", nil
	}
	return string(buf), nil
}

func (r *Read) lines(ctx context.Context, f *os.File, win window) (string, error) {
	end := win.offset + win.limit // exclusive line number
	line := 1
	var out []byte
	chunk := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.New("read timed out")
		}
		n, err := f.Read(chunk)
		for _, c := range chunk[:n] {
			if line >= end {
				return string(out) + fmt.Sprintf("\n[more lines: continue with offset %d]", end), nil
			}
			if line >= win.offset {
				if int64(len(out)) >= r.MaxBytes {
					return string(out) + fmt.Sprintf("\n[truncated at %d bytes: continue with offset %d]", r.MaxBytes, line), nil
				}
				out = append(out, c)
			}
			if c == '\n' {
				line++
			}
		}
		if errors.Is(err, io.EOF) {
			return string(out), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// NoSymlinkPath checks that every component of rel under root is a real
// directory or file, never a symlink, and returns an error otherwise. grep
// and similar children follow a symlink given as an operand, so the path is
// vetted here before it is handed over.
func NoSymlinkPath(root, rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	dirfd, err := unix.Open(real, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." {
			continue
		}
		if c == ".." {
			unix.Close(dirfd)
			return errors.New("path contains ..")
		}
		fd, err := unix.Openat(dirfd, c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		unix.Close(dirfd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
				return errors.New("symbolic links are not followed")
			}
			return err
		}
		dirfd = fd
	}
	unix.Close(dirfd)
	return nil
}
