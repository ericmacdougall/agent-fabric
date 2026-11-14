package checkout

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrCheckout refuses a worktree that could not be built exactly as AC-23
// requires. Nothing is launched after it.
var ErrCheckout = errors.New("checkout refused")

// Limits bound what one extraction may write (resource limits).
const (
	MaxEntries = 20000     // archive entries, directories included
	MaxBytes   = 512 << 20 // file content
	// MaxListBytes bounds the file list handed to the result scan (W2
	// AC-10): the sum of the listed path lengths.
	MaxListBytes = 4 << 20
)

// Result describes a built worktree.
type Result struct {
	// Files are the relative paths of the regular files git archive wrote,
	// sorted, taken from the archive stream fabd read itself, never from a
	// walk of the directory (which the worker could have seen by then).
	Files []string
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitConfigArgs are passed to every git invocation: no repo-configured hook
// or fsmonitor program ever runs (AC-23, NEG-17).
var GitConfigArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// gitEnv is the whole environment of a git child: no system or global
// config, no inherited GIT_* variable, no prompt.
var gitEnv = []string{
	"PATH=/usr/bin:/bin",
	"HOME=/nonexistent",
	"LC_ALL=C",
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_NO_REPLACE_OBJECTS=1",
}

// Extractor builds worktrees. Git is the absolute path of the git binary.
type Extractor struct {
	Git  string
	Stat Stater
	Who  Principals
	// run, when set, replaces exec for tests that inspect the git argv.
	run func(ctx context.Context, dir string, args []string, stdout io.Writer) error
}

// GitArgv is the argv (after the git binary) of one git call in mirror.
func GitArgv(mirror string, sub ...string) []string {
	a := append([]string{"--git-dir=" + mirror}, GitConfigArgs...)
	return append(a, sub...)
}

func (e Extractor) git(ctx context.Context, mirror string, stdout io.Writer, sub ...string) error {
	args := GitArgv(mirror, sub...)
	if e.run != nil {
		return e.run(ctx, mirror, args, stdout)
	}
	cmd := exec.CommandContext(ctx, e.Git, args...)
	cmd.Dir = mirror // never a directory the worker could write (I-7)
	cmd.Env = gitEnv
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", sub[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k]) //nolint:errcheck // a bytes.Buffer
		l.n -= k
	}
	return len(p), nil
}

// CheckMirror refuses a mirror the worker could change: the directory and
// every ancestor, and its config file, must be operator- or root-owned and
// not writable by the worker.
func (e Extractor) CheckMirror(mirror string) error {
	if err := e.Who.Validate(); err != nil {
		return err
	}
	if err := CheckAncestors(e.Stat, e.Who, mirror, "mirror"); err != nil {
		return err
	}
	i, err := e.Stat.Lstat(filepath.Join(mirror, "config"))
	if err != nil {
		return fmt.Errorf("%w: mirror config cannot be checked: %v", ErrOwnership, err)
	}
	if !i.Mode.IsRegular() {
		return fmt.Errorf("%w: mirror config is not a regular file", ErrOwnership)
	}
	return e.Who.CheckOwned(i, "mirror config")
}

// Extract writes the tree of commit sha from mirror into dest, a directory
// that must not exist yet and whose parent passes CheckAncestors. On any
// error dest is removed and nothing is left behind.
func (e Extractor) Extract(ctx context.Context, mirror, sha, dest string) (res Result, err error) {
	if !fullSHA.MatchString(sha) {
		return Result{}, fmt.Errorf("%w: %q is not a full 40-hex sha", ErrCheckout, sha)
	}
	if e.Git == "" || !filepath.IsAbs(e.Git) {
		return Result{}, fmt.Errorf("%w: git must be an absolute path", ErrCheckout)
	}
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
		return Result{}, fmt.Errorf("%w: destination is not an absolute clean path", ErrCheckout)
	}
	if err := e.CheckMirror(mirror); err != nil {
		return Result{}, err
	}
	if err := CheckAncestors(e.Stat, e.Who, filepath.Dir(dest), "worktree parent"); err != nil {
		return Result{}, err
	}
	var out bytes.Buffer
	if err := e.git(ctx, mirror, &out, "rev-parse", "--verify", "--end-of-options", sha+"^{commit}"); err != nil {
		return Result{}, fmt.Errorf("%w: %s is not a commit in the mirror: %v", ErrCheckout, sha, err)
	}
	if got := strings.TrimSpace(out.String()); got != sha {
		return Result{}, fmt.Errorf("%w: sha mismatch: the mirror resolves %s to %q", ErrCheckout, sha, got)
	}
	// 0700 while it is written: the worker cannot enter a half-built tree.
	if err := os.Mkdir(dest, 0o700); err != nil {
		return Result{}, fmt.Errorf("%w: worktree directory not created fresh: %v", ErrCheckout, err)
	}
	defer func() {
		if err != nil {
			RemoveTree(dest) //nolint:errcheck // best effort; the error already refuses the launch
		}
	}()
	pr, pw := io.Pipe()
	gitErr := make(chan error, 1)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		err := e.git(cctx, mirror, pw, "archive", "--format=tar", "--end-of-options", sha)
		pw.CloseWithError(err)
		gitErr <- err
	}()
	modes, files, xerr := extractTar(pr, dest, sha)
	if xerr != nil {
		cancel()
		pr.CloseWithError(xerr)
		<-gitErr
		return Result{}, xerr
	}
	io.Copy(io.Discard, pr) //nolint:errcheck // drain trailing padding
	if gerr := <-gitErr; gerr != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrCheckout, gerr)
	}
	if err := seal(dest, modes); err != nil {
		return Result{}, err
	}
	if err := e.Verify(dest); err != nil {
		return Result{}, err
	}
	return Result{Files: files}, nil
}

// extractTar writes regular files and directories from a git archive
// stream inside dest through an os.Root, so no entry can escape it. It
// returns, for each written path, whether git recorded it as executable.
func extractTar(r io.Reader, dest, sha string) (map[string]bool, []string, error) {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrCheckout, err)
	}
	defer root.Close()
	tr := tar.NewReader(r)
	modes := map[string]bool{}
	sawHeader := false
	var total int64
	var files []string
	listBytes := 0
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: archive stream: %v", ErrCheckout, err)
		}
		if n > MaxEntries {
			return nil, nil, fmt.Errorf("%w: more than %d archive entries", ErrCheckout, MaxEntries)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// git archive records the commit id here; a different one is a
			// sha mismatch.
			if hdr.PAXRecords["comment"] != sha {
				return nil, nil, fmt.Errorf("%w: sha mismatch: the archive is of commit %q", ErrCheckout, hdr.PAXRecords["comment"])
			}
			sawHeader = true
			continue
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return nil, nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirs(root, name, modes); err != nil {
				return nil, nil, err
			}
		case tar.TypeReg:
			if total += hdr.Size; hdr.Size < 0 || total > MaxBytes {
				return nil, nil, fmt.Errorf("%w: the tree is larger than %d bytes", ErrCheckout, MaxBytes)
			}
			if d := path.Dir(name); d != "." {
				if err := mkdirs(root, d, modes); err != nil {
					return nil, nil, err
				}
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %s: %v", ErrCheckout, name, err)
			}
			_, cerr := io.CopyN(f, tr, hdr.Size)
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return nil, nil, fmt.Errorf("%w: %s: %v", ErrCheckout, name, cerr)
			}
			modes[name] = hdr.Mode&0o100 != 0
			if listBytes += len(name); listBytes > MaxListBytes {
				return nil, nil, fmt.Errorf("%w: the file list is larger than %d bytes", ErrCheckout, MaxListBytes)
			}
			files = append(files, name)
		default:
			// Symlinks, hard links and devices are refused: a link in a
			// read-only tree could still point a permitted Read at a path
			// outside it.
			return nil, nil, fmt.Errorf("%w: %s has archive type %q; only files and directories are allowed", ErrCheckout, name, string(hdr.Typeflag))
		}
	}
	if !sawHeader {
		return nil, nil, fmt.Errorf("%w: the archive carries no commit id", ErrCheckout)
	}
	sort.Strings(files)
	return modes, files, nil
}

func entryName(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("%w: archive entry %q is not a clean relative path", ErrCheckout, raw)
	}
	return name, nil
}

// mkdirs creates name and its missing parents inside root, recording each
// as a directory.
func mkdirs(root *os.Root, name string, modes map[string]bool) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		if _, isFile := modes[p]; isFile {
			return fmt.Errorf("%w: %s is a file and a directory", ErrCheckout, p)
		}
		if err := root.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s: %v", ErrCheckout, p, err)
		}
		fi, err := root.Lstat(p)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrCheckout, p)
		}
	}
	return nil
}

// seal sets the final modes, deepest first: executable files 0555, other
// files 0444, directories 0555, and dest itself 0555 last.
func seal(dest string, modes map[string]bool) error {
	var dirs []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		rel, _ := filepath.Rel(dest, p)
		mode := os.FileMode(0o444)
		if modes[filepath.ToSlash(rel)] {
			mode = 0o555
		}
		return os.Chmod(p, mode)
	})
	if err != nil {
		return fmt.Errorf("%w: sealing the tree: %v", ErrCheckout, err)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return fmt.Errorf("%w: sealing the tree: %v", ErrCheckout, err)
		}
	}
	return nil
}

// Verify checks a built worktree: every ancestor of dest passes
// CheckAncestors, and every entry of the tree is a directory 0555 or a
// regular file 0444 or 0555, owned by the operator or root, and not
// writable by the worker.
func (e Extractor) Verify(dest string) error {
	if err := CheckAncestors(e.Stat, e.Who, dest, "worktree"); err != nil {
		return err
	}
	return filepath.WalkDir(dest, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCheckout, err)
		}
		i, err := e.Stat.Lstat(p)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCheckout, p, err)
		}
		switch {
		case i.Mode.IsDir():
			if i.Mode.Perm() != 0o555 {
				return fmt.Errorf("%w: directory %s has mode %v, want 0555", ErrCheckout, p, i.Mode.Perm())
			}
		case i.Mode.IsRegular():
			if m := i.Mode.Perm(); m != 0o444 && m != 0o555 {
				return fmt.Errorf("%w: file %s has mode %v, want 0444 or 0555", ErrCheckout, p, m)
			}
		default:
			return fmt.Errorf("%w: %s is neither a file nor a directory", ErrCheckout, p)
		}
		if i.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("%w: %s has a special mode bit", ErrCheckout, p)
		}
		return e.Who.CheckOwned(i, p)
	})
}

// RemoveTree deletes a sealed worktree: it restores the owner's write bit
// on each directory, then removes everything.
func RemoveTree(dir string) error {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // best effort
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700) //nolint:errcheck
		}
		return nil
	})
	return os.RemoveAll(dir)
}
