package mission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/checkout"
	"fabric/internal/domain"
	"fabric/internal/provider/claude"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type prepRig struct {
	base, hooks, roots, mirror, sha, fab, brief string
	p                                           *Preparer
}

func newPrepRig(t *testing.T) *prepRig {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	gitBin, _ = filepath.EvalSymlinks(gitBin)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	t.Cleanup(func() { checkout.RemoveTree(base) })
	r := &prepRig{base: base, hooks: filepath.Join(base, "hooks"), roots: filepath.Join(base, "attempts")}
	os.Mkdir(r.hooks, 0o755)
	os.Mkdir(r.roots, 0o755)
	src := filepath.Join(base, "src")
	os.Mkdir(src, 0o755)
	os.WriteFile(filepath.Join(src, "test.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(src, "lib.sh"), []byte("x=1\n"), 0o644)
	git(t, base, "init", "-q", src)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "canary")
	r.sha = git(t, src, "rev-parse", "HEAD")
	r.mirror = filepath.Join(base, "mirror.git")
	git(t, base, "clone", "-q", "--bare", src, r.mirror)
	// The "sealed fab binary" and its hard link as the gate.
	r.fab = filepath.Join(base, "fab")
	os.WriteFile(r.fab, []byte("\x7fELF sealed fab build"), 0o755)
	if err := os.Link(r.fab, filepath.Join(r.hooks, BashGateName)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("\x7fELF sealed fab build"))
	f := newFixture(t, nil)
	r.brief = f.brief
	r.p = &Preparer{Loader: f.loader(nil), Git: gitBin, AttemptRoot: r.roots, HooksDir: r.hooks, GateSHA: hex.EncodeToString(sum[:])}
	return r
}

func (r *prepRig) config(t *testing.T, sha string) *Config {
	t.Helper()
	d := validDoc(r.brief)
	d["repo"], d["sha"] = r.mirror, sha
	b, _ := json.Marshal(d)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AC-23, AC-24: Prepare builds an operator-owned attempt directory with a
// sealed worktree, a read-only settings file the launch check accepts, and
// the archive's file list.
func TestPrepareBuildsAttemptDir(t *testing.T) {
	r := newPrepRig(t)
	out, err := r.p.Prepare(context.Background(), r.config(t, r.sha), "r_canary_1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Dir != filepath.Join(r.roots, "r_canary_1") || out.Worktree != filepath.Join(out.Dir, "wt") {
		t.Fatalf("layout %+v", out)
	}
	for p, want := range map[string]os.FileMode{out.Dir: 0o555, out.Worktree: 0o555, out.Settings: 0o444,
		filepath.Join(out.Dir, FilesFile): 0o444, filepath.Join(out.Worktree, "test.sh"): 0o555} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, fi.Mode(), err, want)
		}
	}
	if out.Brief != filepath.Join(out.Dir, BriefFile) {
		t.Errorf("brief %q", out.Brief)
	}
	if b, err := os.ReadFile(out.Brief); err != nil || string(b) != "diagnose the failing test\n" {
		t.Errorf("brief copy %q %v", b, err)
	}
	if strings.Join(out.Files, ",") != "lib.sh,test.sh" {
		t.Errorf("files %v", out.Files)
	}
	w := workerUID
	if err := claude.CheckSettingsFile(out.Settings, r.hooks, &w); err != nil {
		t.Errorf("the launch's settings check refuses the attempt settings: %v", err)
	}
	b, _ := os.ReadFile(out.Settings)
	if !strings.Contains(string(b), "Read(/"+out.Worktree+"/**)") {
		t.Errorf("settings do not scope reads to the worktree")
	}
	// Fresh per attempt: the same attempt cannot be prepared twice.
	if _, err := r.p.Prepare(context.Background(), r.config(t, r.sha), "r_canary_1"); err == nil {
		t.Error("second Prepare of one attempt succeeded")
	}
}

// AC-23: an extraction failure leaves no attempt directory.
func TestPrepareFailureLeavesNothing(t *testing.T) {
	r := newPrepRig(t)
	if _, err := r.p.Prepare(context.Background(), r.config(t, strings.Repeat("ab", 20)), "r_canary_2"); !errors.Is(err, checkout.ErrCheckout) {
		t.Fatalf("absent sha: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.roots, "r_canary_2")); !os.IsNotExist(err) {
		t.Fatal("attempt directory left behind")
	}
	r.p.Loader.Stat = fakeStat{over: map[string]func(checkout.Info) checkout.Info{r.roots: func(i checkout.Info) checkout.Info { i.UID = workerUID; return i }}}
	if _, err := r.p.Prepare(context.Background(), r.config(t, r.sha), "r_canary_3"); !errors.Is(err, checkout.ErrOwnership) {
		t.Fatalf("worker-owned attempt root: %v", err)
	}
	for _, bad := range []string{"../r_x_1", "r_x_1/..", ""} {
		if _, err := r.p.Prepare(context.Background(), r.config(t, r.sha), domain.AttemptID(bad)); err == nil {
			t.Errorf("attempt id %q accepted", bad)
		}
	}
}

// Owner ruling: the gate executable is verified like the provider pin
// before any settings file names it; a failure refuses the launch.
func TestPrepareVerifiesGate(t *testing.T) {
	gate := func(r *prepRig) string { return filepath.Join(r.hooks, BashGateName) }
	for name, mutate := range map[string]func(r *prepRig){
		"swapped gate binary": func(r *prepRig) {
			os.Remove(gate(r))
			os.WriteFile(gate(r), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		},
		"gate in a worker-writable dir": func(r *prepRig) {
			r.p.Loader.Stat = fakeStat{over: map[string]func(checkout.Info) checkout.Info{r.hooks: func(i checkout.Info) checkout.Info {
				i.GID = workerGID
				i.Mode = os.ModeDir | 0o775
				return i
			}}}
		},
		"gate owned by the worker": func(r *prepRig) {
			r.p.Loader.Stat = fakeStat{over: map[string]func(checkout.Info) checkout.Info{gate(r): func(i checkout.Info) checkout.Info { i.UID = workerUID; return i }}}
		},
		"symlinked gate": func(r *prepRig) {
			os.Remove(gate(r))
			os.Symlink(r.fab, gate(r))
		},
		"missing gate":  func(r *prepRig) { os.Remove(gate(r)) },
		"no pinned sha": func(r *prepRig) { r.p.GateSHA = "" },
		"non-exec gate": func(r *prepRig) { os.Chmod(r.fab, 0o644) },
	} {
		r := newPrepRig(t)
		mutate(r)
		if _, err := r.p.Prepare(context.Background(), r.config(t, r.sha), "r_canary_9"); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
		if _, err := os.Lstat(filepath.Join(r.roots, "r_canary_9")); !os.IsNotExist(err) {
			t.Errorf("%s: attempt directory created", name)
		}
	}
}
