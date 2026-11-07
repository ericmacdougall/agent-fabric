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

func (r *Read) Name() string        { return "read" }
func (r *Read) SideEffecting() bool { return false }
func (r *Read) Def() json.RawMessage {
	return json.RawMessage(`{"name":"read","description":"Read a regular text file inside the worktree. Path is relative to the worktree root.","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`)
}

func (r *Read) Run(ctx context.Context, input json.RawMessage) (string, bool) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Path == "" {
		return "read needs {\"path\": \"relative/file\"}", true
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	out, err := r.read(ctx, in.Path)
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

func (r *Read) read(ctx context.Context, p string) (string, error) {
	parts, err := r.rel(p)
	if err != nil {
		return "", err
	}
	dirfd, err := unix.Open(r.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
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
