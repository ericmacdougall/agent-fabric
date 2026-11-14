package checkout

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	workerUID = 4242001
	workerGID = 4242002
	thirdUID  = 4242003
)

// fakeStat is OS with chosen owners and modes substituted per path: tests
// do not run as root, so ownership by the worker or a third account can only
// be simulated.
type fakeStat struct {
	over map[string]func(Info) Info
}

func (f fakeStat) apply(p string, i Info) Info {
	if fn, ok := f.over[p]; ok {
		return fn(i)
	}
	return i
}

func (f fakeStat) Lstat(p string) (Info, error) {
	i, err := OS{}.Lstat(p)
	if err != nil {
		return i, err
	}
	return f.apply(p, i), nil
}

func (f fakeStat) Fstat(file *os.File) (Info, error) {
	i, err := OS{}.Fstat(file)
	if err != nil {
		return i, err
	}
	return f.apply(file.Name(), i), nil
}

func who() Principals {
	return Principals{OperatorUID: uint32(os.Getuid()), WorkerUID: workerUID, WorkerGIDs: []uint32{workerGID}}
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Sealed trees have 0555 directories; restore write so cleanup works.
	t.Cleanup(func() { RemoveTree(d) })
	return d
}

func gitPath(t *testing.T) string {
	t.Helper()
	g, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	g, _ = filepath.EvalSymlinks(g)
	return g
}

func runGit(t *testing.T, dir string, args ...string) string {
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

// fakeMirror builds a canary-like repo (an executable test.sh, a lib.sh, a
// nested file) and returns a bare mirror of it and the commit sha.
func fakeMirror(t *testing.T, base string) (mirror, sha string) {
	t.Helper()
	src := filepath.Join(base, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "test.sh"), []byte("#!/bin/sh\n. ./lib.sh\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(src, "lib.sh"), []byte("x=1\n"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "data.txt"), []byte("data\n"), 0o644)
	runGit(t, base, "init", "-q", src)
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-q", "-m", "canary")
	sha = runGit(t, src, "rev-parse", "HEAD")
	mirror = filepath.Join(base, "mirror.git")
	runGit(t, base, "clone", "-q", "--bare", src, mirror)
	return mirror, sha
}

func extractor(t *testing.T, st Stater) Extractor {
	if st == nil {
		st = OS{}
	}
	return Extractor{Git: gitPath(t), Stat: st, Who: who()}
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// AC-23: a fresh read-only tree per attempt, executable files 0555, others
// 0444, directories 0555, contents exactly the commit's.
func TestExtractFreshReadOnlyTree(t *testing.T) {
	base := realDir(t)
	mirror, sha := fakeMirror(t, base)
	work := filepath.Join(base, "attempts")
	os.Mkdir(work, 0o755)
	e := extractor(t, nil)
	wt := filepath.Join(work, "r_a_1")
	res, err := e.Extract(context.Background(), mirror, sha, wt)
	if err != nil {
		t.Fatal(err)
	}
	// W2 AC-10: the file list comes from the archive stream, sorted.
	if got := strings.Join(res.Files, ","); got != "lib.sh,sub/data.txt,test.sh" {
		t.Errorf("Files = %q", got)
	}
	for p, want := range map[string]os.FileMode{
		wt: 0o555, filepath.Join(wt, "sub"): 0o555,
		filepath.Join(wt, "test.sh"): 0o555, filepath.Join(wt, "lib.sh"): 0o444, filepath.Join(wt, "sub", "data.txt"): 0o444,
	} {
		if got := perm(t, p); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "test.sh")); string(b) != "#!/bin/sh\n. ./lib.sh\nexit 1\n" {
		t.Errorf("test.sh content %q", b)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); !os.IsNotExist(err) {
		t.Errorf("worktree has a .git: %v", err)
	}
	// Fresh: the same destination is never reused.
	if _, err := e.Extract(context.Background(), mirror, sha, wt); !errors.Is(err, ErrCheckout) {
		t.Errorf("re-extract into an existing dir: %v, want ErrCheckout", err)
	}
	if got := perm(t, filepath.Join(wt, "test.sh")); got != 0o555 {
		t.Errorf("refused re-extract changed the earlier tree: %v", got)
	}
	// A second attempt gets its own tree.
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(work, "r_a_2")); err != nil {
		t.Fatal(err)
	}
}

// AC-23: a sha the mirror does not resolve to itself refuses, leaving
// nothing behind.
func TestExtractSHAMismatch(t *testing.T) {
	base := realDir(t)
	mirror, sha := fakeMirror(t, base)
	blob := runGit(t, mirror, "rev-parse", sha+":lib.sh")
	e := extractor(t, nil)
	for name, bad := range map[string]string{
		"absent commit": strings.Repeat("ab", 20),
		"a blob":        blob,
		"short sha":     sha[:12],
		"upper case":    strings.ToUpper(sha),
		"option":        "--output=/x" + strings.Repeat("0", 29),
	} {
		dest := filepath.Join(base, "wt-"+strings.ReplaceAll(name, " ", "-"))
		if _, err := e.Extract(context.Background(), mirror, bad, dest); !errors.Is(err, ErrCheckout) {
			t.Errorf("%s: %v, want ErrCheckout", name, err)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Errorf("%s: destination left behind", name)
		}
	}
	// A mirror whose rev-parse answers a different commit (a replaced or
	// tampered object) refuses too.
	other := strings.Repeat("cd", 20)
	e.run = func(ctx context.Context, dir string, args []string, out io.Writer) error {
		if args[len(args)-3] == "--verify" || contains(args, "rev-parse") {
			io.WriteString(out, other+"\n")
			return nil
		}
		return errors.New("archive must not run")
	}
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(base, "wt-tampered")); err == nil || !strings.Contains(err.Error(), "sha mismatch") {
		t.Errorf("rev-parse answering another commit: %v, want sha mismatch", err)
	}
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

// AC-23: every ancestor of the mirror and of the worktree must be owned by
// the operator or root and not writable by the worker.
func TestExtractRefusesAncestors(t *testing.T) {
	base := realDir(t)
	mirror, sha := fakeMirror(t, base)
	work := filepath.Join(base, "attempts")
	os.Mkdir(work, 0o755)
	cases := map[string]func(Info) Info{
		"owned by a third user":         func(i Info) Info { i.UID = thirdUID; return i },
		"owned by the worker":           func(i Info) Info { i.UID = workerUID; i.Mode = os.ModeDir | 0o755; return i },
		"group-writable, worker group":  func(i Info) Info { i.GID = workerGID; i.Mode = os.ModeDir | 0o775; return i },
		"other-writable":                func(i Info) Info { i.Mode = os.ModeDir | 0o757; return i },
		"sticky world-writable by root": func(i Info) Info { i.UID = 0; i.Mode = os.ModeDir | os.ModeSticky | 0o777; return i },
		"a symlink":                     func(i Info) Info { i.Mode = os.ModeSymlink | 0o777; return i },
	}
	for name, fn := range cases {
		for _, target := range []struct{ what, path string }{{"mirror", mirror}, {"mirror ancestor", base}, {"worktree parent", work}} {
			e := extractor(t, fakeStat{over: map[string]func(Info) Info{target.path: fn}})
			dest := filepath.Join(work, "r_x")
			_, err := e.Extract(context.Background(), mirror, sha, dest)
			if !errors.Is(err, ErrOwnership) {
				t.Errorf("%s %s: %v, want ErrOwnership", target.what, name, err)
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("%s %s: destination created", target.what, name)
			}
		}
	}
	// Control: group-writable by a group the worker is not in passes.
	e := extractor(t, fakeStat{over: map[string]func(Info) Info{work: func(i Info) Info { i.GID = 4242999; i.Mode = os.ModeDir | 0o775; return i }}})
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(work, "r_ok")); err != nil {
		t.Errorf("group-writable by a non-worker group: %v", err)
	}
	// A worker-writable mirror config refuses.
	e = extractor(t, fakeStat{over: map[string]func(Info) Info{filepath.Join(mirror, "config"): func(i Info) Info { i.Mode = 0o666; return i }}})
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(work, "r_cfg")); !errors.Is(err, ErrOwnership) {
		t.Errorf("worker-writable mirror config: %v", err)
	}
	// Operator equal to worker can separate nothing.
	e = extractor(t, nil)
	e.Who.WorkerUID = e.Who.OperatorUID
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(work, "r_same")); !errors.Is(err, ErrOwnership) {
		t.Errorf("worker == operator: %v", err)
	}
}

// AC-23: Verify refuses a tree entry the worker owns or can write.
func TestVerifyRefusesTreeEntries(t *testing.T) {
	base := realDir(t)
	mirror, sha := fakeMirror(t, base)
	wt := filepath.Join(base, "wt")
	if _, err := extractor(t, nil).Extract(context.Background(), mirror, sha, wt); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		path string
		fn   func(Info) Info
	}{
		"test.sh owned by the worker": {filepath.Join(wt, "test.sh"), func(i Info) Info { i.UID = workerUID; return i }},
		"lib.sh other-writable":       {filepath.Join(wt, "lib.sh"), func(i Info) Info { i.Mode = 0o446; return i }},
		"sub owned by a third user":   {filepath.Join(wt, "sub"), func(i Info) Info { i.UID = thirdUID; return i }},
		"data.txt group-writable":     {filepath.Join(wt, "sub", "data.txt"), func(i Info) Info { i.GID = workerGID; i.Mode = 0o464; return i }},
		"test.sh setuid":              {filepath.Join(wt, "test.sh"), func(i Info) Info { i.Mode |= os.ModeSetuid; return i }},
	} {
		e := extractor(t, fakeStat{over: map[string]func(Info) Info{c.path: c.fn}})
		if err := e.Verify(wt); err == nil {
			t.Errorf("%s: Verify passed", name)
		}
	}
	if err := extractor(t, nil).Verify(wt); err != nil {
		t.Errorf("control: %v", err)
	}
}

// AC-23 / NEG-17: git runs only in the mirror, with hooks and fsmonitor off,
// and a trap planted in an earlier tree or in the mirror never executes.
func TestNoHookOrFsmonitorRuns(t *testing.T) {
	base := realDir(t)
	mirror, sha := fakeMirror(t, base)
	marker := filepath.Join(base, "TRAP-FIRED")
	trap := filepath.Join(base, "trap.sh")
	os.WriteFile(trap, []byte("#!/bin/sh\necho fired >> "+marker+"\nexit 0\n"), 0o755)

	// An earlier attempt's tree, writable as the worker's would be, carrying
	// a repo with an fsmonitor and every hook pointing at the trap.
	earlier := filepath.Join(base, "earlier")
	runGit(t, base, "init", "-q", earlier)
	runGit(t, earlier, "config", "core.fsmonitor", trap)
	hooks := filepath.Join(earlier, ".git", "hooks")
	for _, h := range []string{"pre-commit", "post-checkout", "reference-transaction", "pre-auto-gc"} {
		os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\n"+trap+"\n"), 0o755)
	}
	// The mirror itself poisoned the same way.
	runGit(t, mirror, "config", "core.fsmonitor", trap)
	runGit(t, mirror, "config", "core.hooksPath", hooks)

	// Trap armed: git run the broken way (inside the earlier tree, without
	// the overrides) does fire it. Without this the test could pass vacuously.
	os.WriteFile(filepath.Join(earlier, "f"), []byte("x"), 0o644)
	runGit(t, earlier, "status")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("trap not armed: a broken variant (git status in the earlier tree) did not fire it")
	}
	os.Remove(marker)

	var calls [][]string
	e := extractor(t, nil)
	real := e
	e.run = func(ctx context.Context, dir string, args []string, out io.Writer) error {
		calls = append(calls, append([]string{dir}, args...))
		r := real
		r.run = nil
		cmd := exec.CommandContext(ctx, r.Git, args...)
		cmd.Dir, cmd.Env, cmd.Stdout = dir, gitEnv, out
		return cmd.Run()
	}
	if _, err := e.Extract(context.Background(), mirror, sha, filepath.Join(base, "wt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a planted hook or fsmonitor executed during extraction")
	}
	if len(calls) != 2 {
		t.Fatalf("git calls %d, want 2 (rev-parse, archive)", len(calls))
	}
	for _, c := range calls {
		if c[0] != mirror || c[1] != "--git-dir="+mirror {
			t.Errorf("git ran outside the mirror: dir %q args %q", c[0], c[1:])
		}
		joined := strings.Join(c[1:], " ")
		if !strings.Contains(joined, "-c core.hooksPath=/dev/null -c core.fsmonitor=false") {
			t.Errorf("git call without the overrides: %s", joined)
		}
	}
	for _, v := range gitEnv {
		if strings.HasPrefix(v, "GIT_DIR=") || strings.HasPrefix(v, "GIT_WORK_TREE=") {
			t.Errorf("git env carries %s", v)
		}
	}
}

func tarOf(t *testing.T, sha string, entries ...tar.Header) io.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if sha != "" {
		w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}})
	}
	for _, h := range entries {
		h := h
		body := []byte(nil)
		if h.Typeflag == tar.TypeReg {
			body = []byte("x")
			h.Size = 1
		}
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	w.Close()
	return &b
}

// The archive stream is untrusted: links, escapes and a missing or wrong
// commit id are refused.
func TestExtractTarRefusals(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	reg := func(n string) tar.Header { return tar.Header{Typeflag: tar.TypeReg, Name: n, Mode: 0o644} }
	for name, r := range map[string]io.Reader{
		"symlink":         tarOf(t, sha, tar.Header{Typeflag: tar.TypeSymlink, Name: "env", Linkname: "/proc/self/environ"}),
		"hard link":       tarOf(t, sha, reg("a"), tar.Header{Typeflag: tar.TypeLink, Name: "b", Linkname: "a"}),
		"dotdot":          tarOf(t, sha, reg("../escape")),
		"nested dotdot":   tarOf(t, sha, reg("a/../../escape")),
		"absolute":        tarOf(t, sha, reg("/etc/x")),
		"device":          tarOf(t, sha, tar.Header{Typeflag: tar.TypeChar, Name: "dev"}),
		"fifo":            tarOf(t, sha, tar.Header{Typeflag: tar.TypeFifo, Name: "p"}),
		"no commit id":    tarOf(t, "", reg("a")),
		"other commit id": tarOf(t, strings.Repeat("cd", 20), reg("a")),
		"duplicate file":  tarOf(t, sha, reg("a"), reg("a")),
		"file then dir":   tarOf(t, sha, reg("a"), reg("a/b")),
	} {
		dest := filepath.Join(realDir(t), "d")
		os.Mkdir(dest, 0o700)
		if _, _, err := extractTar(r, dest, sha); !errors.Is(err, ErrCheckout) {
			t.Errorf("%s: %v, want ErrCheckout", name, err)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "escape")); !os.IsNotExist(err) {
			t.Errorf("%s: wrote outside dest", name)
		}
	}
	dest := filepath.Join(realDir(t), "d")
	os.Mkdir(dest, 0o700)
	if _, _, err := extractTar(tarOf(t, sha, reg("a/b"), tar.Header{Typeflag: tar.TypeReg, Name: "x.sh", Mode: 0o755}), dest, sha); err != nil {
		t.Errorf("control: %v", err)
	}
}

// W2 AC-10 caps: at most MaxEntries entries and MaxListBytes of listed
// paths; over either the extraction is refused.
func TestExtractListCaps(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	many := func(n, nameLen int) io.Reader {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}})
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("%0*d", nameLen, i)
			w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644})
		}
		w.Close()
		return &b
	}
	run := func(r io.Reader) ([]string, error) {
		dest := filepath.Join(realDir(t), "d")
		os.Mkdir(dest, 0o700)
		_, files, err := extractTar(r, dest, sha)
		return files, err
	}
	if files, err := run(many(MaxEntries, 8)); err != nil || len(files) != MaxEntries {
		t.Fatalf("%d entries: %d files, %v", MaxEntries, len(files), err)
	}
	if _, err := run(many(MaxEntries+1, 8)); !errors.Is(err, ErrCheckout) {
		t.Errorf("%d entries: %v, want ErrCheckout", MaxEntries+1, err)
	}
	// 20000 names of 220 bytes are 4.4 MB > 4 MiB.
	if _, err := run(many(MaxEntries, 220)); err == nil || !strings.Contains(err.Error(), "file list") {
		t.Errorf("oversized list: %v", err)
	}
}
