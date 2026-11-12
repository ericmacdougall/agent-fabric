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

func (g *GitRef) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	full := append([]string{"--git-dir=" + g.GitDir, "--work-tree=" + g.WorkTree, "--literal-pathspecs"}, gitSafetyArgs...)
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Dir = g.WorkTree
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	return cmd.Output()
}

// Tracked reports whether rel is in the index. It errors (callers then treat
// the file as untracked and keep a copy) when the repository configuration
// carries a risky key, when git fails, or on timeout.
func (g *GitRef) Tracked(ctx context.Context, rel string) (bool, error) {
	if err := g.same(); err != nil {
		return false, err
	}
	out, err := g.run(ctx, "config", "--show-scope", "--name-only", "--get-regexp", riskyConfig)
	var ee *exec.ExitError
	switch {
	case err == nil:
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if scope, _, ok := strings.Cut(line, "\t"); ok && scope != "command" {
				return false, errors.New("repository configuration is risky")
			}
		}
	case errors.As(err, &ee) && ee.ExitCode() == 1:
	default:
		return false, fmt.Errorf("git config: %w", err)
	}
	_, err = g.run(ctx, "ls-files", "--error-unmatch", "--", rel)
	if err == nil {
		return true, nil
	}
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-files: %w", err)
}

// PreimageKeeper keeps bounded pre-image copies and per-call journals in a
// runner-only directory (m2, FN-1).
type PreimageKeeper struct {
	Dir *RunnerDir
	Git *GitRef // nil: every existing target is treated as untracked
}

// NeedsCopy: untracked, ignored, or the query failed or was refused.
func (k *PreimageKeeper) NeedsCopy(ctx context.Context, rel string) bool {
	if k == nil || k.Dir == nil {
		return false
	}
	if k.Git == nil {
		return true
	}
	tracked, err := k.Git.Tracked(ctx, rel)
	return err != nil || !tracked
}

// Keep stores data (at most MaxFileBytes) and returns its name.
func (k *PreimageKeeper) Keep(data []byte) (string, error) {
	if len(data) > MaxFileBytes {
		return "", errors.New("pre-image too large")
	}
	f, name, err := k.Dir.Create("pre-")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		k.Dir.Abort(name)
		return "", err
	}
	f.Close()
	k.Dir.Done(name, int64(len(data)))
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
