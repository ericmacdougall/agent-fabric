package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// gitSafetyArgs mirror internal/effects/publish.go gitSafety for the settings
// that matter to a read-only query: no hooks, no fsmonitor, no helpers.
var gitSafetyArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.askPass=",
	"-c", "credential.helper=",
	"-c", "gc.auto=0",
	"-c", "core.commitGraph=false",
	"-c", "protocol.allow=never",
}

// riskyConfig is publish.go's refusal pattern.
const riskyConfig = `^(url\..*|credential\..*|include\..*|includeif\..*|http\..*|core\.(askpass|gitproxy|sshcommand|worktree|alternaterefscommand))$`

// GitRef is the gitdir and worktree recorded once at attempt start (R2-a).
// The git query never trusts the worktree's own .git file afterwards, which
// the confined child can rewrite.
type GitRef struct {
	GitDir   string
	WorkTree string
	dev, ino uint64 // identity of GitDir at attempt start
}

func (g *GitRef) record() (*GitRef, error) {
	var st unix.Stat_t
	if err := unix.Lstat(g.GitDir, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.New("tools: the git directory is not a directory")
	}
	g.dev, g.ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is int32 on darwin
	return g, nil
}

// same refuses a gitdir that was replaced (by a gitfile, a symlink or another
// directory) since it was recorded: git would follow the replacement.
func (g *GitRef) same() error {
	var st unix.Stat_t
	if err := unix.Lstat(g.GitDir, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != g.dev || st.Ino != g.ino { //nolint:unconvert
		return errors.New("the recorded git directory was replaced")
	}
	return nil
}

// NewGitRef records worktree's git directory by reading its .git entry (a
// directory, or a file "gitdir: path"). It runs no git.
func NewGitRef(worktree string) (*GitRef, error) {
	dotgit := filepath.Join(worktree, ".git")
	st, err := os.Lstat(dotgit)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return (&GitRef{GitDir: dotgit, WorkTree: worktree}).record()
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("tools: .git is neither a directory nor a file")
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return nil, err
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return nil, errors.New("tools: .git file has no gitdir line")
	}
	gd := strings.TrimSpace(line)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(worktree, gd)
	}
	return (&GitRef{GitDir: filepath.Clean(gd), WorkTree: worktree}).record()
}

// gitRefRecord is the per-attempt gitdir record (goodqa#15361 F4). It lives
// in the attempt's runner-only pre-image directory (0700, our uid, never under
// RunDir), so the confined child can neither write nor replace it.
const gitRefRecord = "gitref"

type gitRefRec struct {
	V        int    `json:"v"`
	WorkTree string `json:"worktree"`
	None     bool   `json:"none,omitempty"` // the worktree had no usable .git at attempt start
	GitDir   string `json:"gitdir,omitempty"`
	Dev      uint64 `json:"dev,omitempty"`
	Ino      uint64 `json:"ino,omitempty"`
}

// ErrGitDirChanged: the gitdir recorded when the attempt started is gone or
// was replaced. The caller must not trust git for this attempt.
var ErrGitDirChanged = errors.New("the git directory recorded at attempt start changed")

// AttemptGitRef returns the attempt's git reference, recorded once per
// attempt rather than per runner start. The first call reads worktree/.git
// (NewGitRef) and writes the record (path, device and inode, or "none");
// every later call, a resume included, uses only the record and never reads
// .git again, which the child can rewrite between sessions. A recorded gitdir
// that was replaced since is refused with ErrGitDirChanged. A nil reference
// with a nil error means the attempt started without git: every existing
// target is then copied.
func AttemptGitRef(dir *RunnerDir, worktree string) (*GitRef, error) {
	b, err := dir.readRecord(gitRefRecord)
	switch {
	case err == nil:
		var r gitRefRec
		if json.Unmarshal(b, &r) != nil || r.V != 1 {
			return nil, errors.New("tools: the attempt's gitdir record is unreadable")
		}
		if r.WorkTree != worktree {
			return nil, fmt.Errorf("%w: the record names another worktree", ErrGitDirChanged)
		}
		if r.None {
			return nil, nil
		}
		g := &GitRef{GitDir: r.GitDir, WorkTree: r.WorkTree, dev: r.Dev, ino: r.Ino}
		if err := g.same(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGitDirChanged, err)
		}
		return g, nil
	case errors.Is(err, ErrExpired):
		g, gerr := NewGitRef(worktree)
		r := gitRefRec{V: 1, WorkTree: worktree, None: gerr != nil}
		if gerr == nil {
			r.GitDir, r.Dev, r.Ino = g.GitDir, g.dev, g.ino
		}
		out, _ := json.Marshal(r)
		if err := dir.writeRecord(gitRefRecord, out); err != nil {
			return nil, fmt.Errorf("tools: cannot record the attempt's gitdir: %w", err)
		}
		if gerr != nil {
			return nil, nil
		}
		return g, nil
	default:
		return nil, fmt.Errorf("tools: the attempt's gitdir record: %w", err)
	}
}

func (g *GitRef) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	full := append([]string{"--git-dir=" + g.GitDir, "--work-tree=" + g.WorkTree, "--literal-pathspecs"}, gitSafetyArgs...)
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Dir = g.WorkTree
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	return cmd.Output()
}

// checkConfig refuses a repository whose own configuration carries a risky
// key (publish.go's pattern): git would act on it even for a query.
func (g *GitRef) checkConfig(ctx context.Context) error {
	out, err := g.run(ctx, nil, "config", "--show-scope", "--name-only", "--get-regexp", riskyConfig)
	var ee *exec.ExitError
	switch {
	case err == nil:
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if scope, _, ok := strings.Cut(line, "\t"); ok && scope != "command" {
				return errors.New("repository configuration is risky")
			}
		}
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return nil
	default:
		return fmt.Errorf("git config: %w", err)
	}
}

// CleanBlob reports whether data (the exact bytes about to be replaced) is
// already held by git: its blob id (git hash-object --no-filters, in the
// repository's own object format) equals both the stage-0 index entry of rel
// and HEAD:rel. Only then can git restore those bytes, and it returns that
// blob id (goodqa#15361 F1). Anything else is "not clean": untracked, a dirty
// tracked file, a staged-only change, a conflict, no HEAD commit, a clean
// filter that changes the bytes, a replaced gitdir, risky configuration, a
// git failure or a timeout. Callers keep a copy whenever ok is false.
func (g *GitRef) CleanBlob(ctx context.Context, rel string, data []byte) (blob string, ok bool, err error) {
	if err := g.same(); err != nil {
		return "", false, err
	}
	if err := g.checkConfig(ctx); err != nil {
		return "", false, err
	}
	out, err := g.run(ctx, nil, "ls-files", "-s", "-z", "--", rel)
	if err != nil {
		return "", false, fmt.Errorf("git ls-files: %w", err)
	}
	entries := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(out) == 0 || len(entries) != 1 {
		return "", false, nil // untracked, or a conflict (several stages)
	}
	// "<mode> <oid> <stage>\t<path>"
	meta, path, cut := strings.Cut(entries[0], "\t")
	f := strings.Fields(meta)
	if !cut || path != rel || len(f) != 3 || f[2] != "0" || (f[0] != "100644" && f[0] != "100755") {
		return "", false, nil
	}
	index := f[1]
	head, err := g.run(ctx, nil, "rev-parse", "--verify", "--quiet", "HEAD:"+rel)
	if err != nil {
		return "", false, nil // no HEAD commit, or rel is not in it
	}
	disk, err := g.run(ctx, data, "hash-object", "--no-filters", "--stdin")
	if err != nil {
		return "", false, fmt.Errorf("git hash-object: %w", err)
	}
	d := strings.TrimSpace(string(disk))
	if d == "" || d != index || d != strings.TrimSpace(string(head)) {
		return "", false, nil
	}
	return d, true, nil
}

// PreimageKeeper keeps pre-image copies and per-call journals in a
// runner-only directory (m2, FN-1). Copies are never evicted: Dir must come
// from OpenKeepDir, and a full store refuses the write (F2).
type PreimageKeeper struct {
	Dir *RunnerDir
	Git *GitRef // nil: every existing target is copied
	// WritableRoots are the trees the confined child can write (see
	// ChildWritableRoots). The "git already holds these bytes" shortcut is
	// taken only when the recorded gitdir lies outside every one of them: a
	// child that can write the gitdir can commit the bytes, then erase the
	// blob (goodqa#15865, red-team R2). Empty means unknown, so the shortcut is
	// never taken.
	WritableRoots []string
}

// gitShortcutAllowed reports whether git's own copy of a file can stand in for
// a pre-image copy: the gitdir is known and lies outside every child-writable
// root. Anything unresolvable is not allowed.
func (k *PreimageKeeper) gitShortcutAllowed() bool {
	if k.Git == nil || len(k.WritableRoots) == 0 {
		return false
	}
	_, under := UnderAnyRoot(k.Git.GitDir, k.WritableRoots)
	return !under
}

// NeedsCopy reports whether a copy of data (the bytes at rel about to be
// replaced) must be kept. It is false only when the child cannot write the
// gitdir and git already holds exactly those bytes in both the index and
// HEAD; blob then names that git blob, so the result can say where the
// restore comes from.
func (k *PreimageKeeper) NeedsCopy(ctx context.Context, rel string, data []byte) (need bool, blob string) {
	if k == nil || k.Dir == nil {
		return false, ""
	}
	if !k.gitShortcutAllowed() {
		return true, ""
	}
	b, clean, err := k.Git.CleanBlob(ctx, rel, data)
	if err != nil || !clean {
		return true, ""
	}
	return false, b
}

// Keep stores data (at most MaxFileBytes) and returns its name. The space is
// reserved before the file is created; when the store is at its file or byte
// limit it returns ErrPreimageStoreFull and nothing is evicted, so the caller
// refuses the write (goodqa#15361 F2).
func (k *PreimageKeeper) Keep(data []byte) (string, error) {
	if len(data) > MaxFileBytes {
		return "", errors.New("pre-image too large")
	}
	f, name, err := k.Dir.CreateReserved("pre-", int64(len(data)))
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		k.Dir.Abort(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		k.Dir.Abort(name)
		return "", err
	}
	f.Close()
	return name, nil
}

type journalEntry struct {
	Path string `json:"path"`
	Pre  string `json:"pre"`
	Post string `json:"post"`
}

func journalName(tag string) string { return "j-" + tag }

// writeJournal records the pre and intended post digest of a call before the
// rename, so a resume can say whether the write landed. Best effort.
func (k *PreimageKeeper) writeJournal(tag, path, pre, post string) {
	if k == nil || k.Dir == nil || tag == "" {
		return
	}
	b, _ := json.Marshal(journalEntry{path, pre, post})
	fd, err := unix.Openat(k.Dir.fd, journalName(tag), unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "journal")
	_, _ = f.Write(b)
	_ = f.Sync()
	f.Close()
}

func (k *PreimageKeeper) readJournal(tag string) (journalEntry, bool) {
	var e journalEntry
	if k == nil || k.Dir == nil || tag == "" {
		return e, false
	}
	f, err := k.Dir.Open(journalName(tag))
	if err != nil {
		return e, false
	}
	defer f.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(f)
	return e, json.Unmarshal(buf.Bytes(), &e) == nil
}
