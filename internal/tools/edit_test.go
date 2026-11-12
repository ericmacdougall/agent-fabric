package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/tools"
)

type editEnv struct {
	wt, outside, keep string
	rd                *tools.Read
	pin               *tools.WorktreePin
	ed                *tools.Edit
	wr                *tools.Write
	pre               *tools.PreimageKeeper
}

func newEditEnv(t *testing.T) *editEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &editEnv{wt: filepath.Join(base, "wt"), outside: filepath.Join(base, "outside"), keep: filepath.Join(base, "state", "pre")}
	for _, d := range []string{e.wt, e.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.pin, err = tools.PinWorktree(e.wt)
	if err != nil {
		t.Fatal(err)
	}
	e.rd, err = tools.NewRead(e.wt, 64<<10, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rdir, err := tools.OpenKeepDir(e.keep, "pre-", 256, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rdir.Close)
	e.pre = &tools.PreimageKeeper{Dir: rdir}
	e.ed, e.wr = tools.NewEdit(e.rd, e.pin, e.pre), tools.NewWrite(e.rd, e.pin, e.pre)
	return e
}

func (e *editEnv) gitInit(t *testing.T) {
	t.Helper()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = e.wt
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	g, err := tools.NewGitRef(e.wt)
	if err != nil {
		t.Fatal(err)
	}
	e.pre.Git = g
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (e *editEnv) put(t *testing.T, rel, s string) string {
	t.Helper()
	p := filepath.Join(e.wt, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func edits(path string, pairs ...string) json.RawMessage {
	var es []map[string]string
	for i := 0; i < len(pairs); i += 2 {
		es = append(es, map[string]string{"oldText": pairs[i], "newText": pairs[i+1]})
	}
	b, _ := json.Marshal(map[string]any{"path": path, "edits": es})
	return b
}

func wr(path, content string, kv ...string) json.RawMessage {
	m := map[string]string{"path": path, "content": content}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

func mustFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditCRLFBOMTwoEdits(t *testing.T) {
	e := newEditEnv(t)
	orig := "\xef\xbb\xbfa\r\nb\r\nc\r\nd\r\n"
	p := e.put(t, "f.txt", orig)
	out, bad := e.ed.Run(context.Background(), edits("f.txt", "c", "CC", "a\nb", "x\ny"))
	if bad {
		t.Fatal(out)
	}
	want := "\xef\xbb\xbfx\r\ny\r\nCC\r\nd\r\n"
	if got := mustFile(t, p); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, s := range []string{sha([]byte(orig)), sha([]byte(want)), "f.txt", "--- a/f.txt"} {
		if !strings.Contains(out, s) {
			t.Fatalf("result lacks %q: %s", s, out)
		}
	}
}

func TestEditSmartQuoteFallbackKeepsOtherBytes(t *testing.T) {
	e := newEditEnv(t)
	orig := "keep    \nsay “hi” — now\nafter   \nlast"
	p := e.put(t, "q.txt", orig)
	out, bad := e.ed.Run(context.Background(), edits("q.txt", `say "hi" - now`, "SAID"))
	if bad {
		t.Fatal(out)
	}
	if got, want := mustFile(t, p), "keep    \nSAID\nafter   \nlast"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// an exact match must win over a fold: the second text matches only exactly.
	p2 := e.put(t, "q2.txt", "a \"x\" b “x”")
	if out, bad := e.ed.Run(context.Background(), edits("q2.txt", `"x"`, "Y")); bad {
		t.Fatal(out)
	}
	if got := mustFile(t, p2); got != "a Y b “x”" {
		t.Fatalf("exact match must win: %q", got)
	}
}

func TestEditExpectSha(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", "one two")
	bad := strings.Repeat("0", 64)
	b, _ := json.Marshal(map[string]any{"path": "f.txt", "edits": []map[string]string{{"oldText": "one", "newText": "1"}}, "expect_sha256": bad})
	if out, isErr := e.ed.Run(context.Background(), b); !isErr {
		t.Fatalf("mismatched expect_sha256 was accepted: %s", out)
	}
	if mustFile(t, p) != "one two" {
		t.Fatal("file changed")
	}
	b, _ = json.Marshal(map[string]any{"path": "f.txt", "edits": []map[string]string{{"oldText": "one", "newText": "1"}}, "expect_sha256": sha([]byte("one two"))})
	if out, isErr := e.ed.Run(context.Background(), b); isErr {
		t.Fatal(out)
	}
}

func TestEditRefusals(t *testing.T) {
	e := newEditEnv(t)
	orig := "dup\nx\ndup\nabcdef\n"
	p := e.put(t, "f.txt", orig)
	cases := map[string]json.RawMessage{
		"twice":   edits("f.txt", "dup", "z"),
		"zero":    edits("f.txt", "nothere", "z"),
		"overlap": edits("f.txt", "abcd", "1", "cdef", "2"),
		"empty":   edits("f.txt", "", "z"),
		"same":    edits("f.txt", "x", "x"),
	}
	for name, in := range cases {
		out, isErr := e.ed.Run(context.Background(), in)
		if !isErr {
			t.Fatalf("%s: accepted: %s", name, out)
		}
		if mustFile(t, p) != orig {
			t.Fatalf("%s: file changed", name)
		}
		if name == "twice" && (!strings.Contains(out, "2 places") || !strings.Contains(out, "1,3")) {
			t.Fatalf("ambiguity error must name the count and lines: %s", out)
		}
	}
}

func TestEditWriteConfinement(t *testing.T) {
	e := newEditEnv(t)
	secret := filepath.Join(e.outside, "secret.txt")
	os.WriteFile(secret, []byte("outside"), 0o644)
	e.put(t, "real/f.txt", "inside")
	os.Symlink(e.outside, filepath.Join(e.wt, "linkdir"))
	os.Symlink(secret, filepath.Join(e.wt, "linkfile"))
	e.put(t, "h1", "hard")
	os.Link(filepath.Join(e.wt, "h1"), filepath.Join(e.wt, "h2"))
	syscall.Mkfifo(filepath.Join(e.wt, "fifo"), 0o644)
	for _, path := range []string{"../outside/secret.txt", secret, "linkdir/secret.txt", "linkfile", "h1", "h2", "fifo", "real/../../outside/secret.txt", "a\x00b"} {
		if out, isErr := e.wr.Run(context.Background(), wr(path, "PWNED")); !isErr {
			t.Fatalf("write %q accepted: %s", path, out)
		}
		if out, isErr := e.ed.Run(context.Background(), edits(path, "outside", "PWNED")); !isErr {
			t.Fatalf("edit %q accepted: %s", path, out)
		}
	}
	if mustFile(t, secret) != "outside" || mustFile(t, filepath.Join(e.wt, "h1")) != "hard" {
		t.Fatal("something outside the pinned file set changed")
	}
}

func TestEditAfterPinMoved(t *testing.T) {
	e := newEditEnv(t)
	e.put(t, "f.txt", "x")
	moved := e.wt + "-moved"
	if err := os.Rename(e.wt, moved); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(e.wt, 0o755)
	os.WriteFile(filepath.Join(e.wt, "f.txt"), []byte("x"), 0o644)
	if out, isErr := e.wr.Run(context.Background(), wr("f.txt", "y")); !isErr || !strings.Contains(out, "worktree moved") {
		t.Fatalf("write after the pin moved: %v %s", isErr, out)
	}
	if mustFile(t, filepath.Join(e.wt, "f.txt")) != "x" {
		t.Fatal("write landed in the swapped-in directory")
	}
}

func TestWriteRefusesGitDir(t *testing.T) {
	e := newEditEnv(t)
	os.MkdirAll(filepath.Join(e.wt, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(e.wt, ".git", "config"), []byte("c"), 0o644)
	for _, p := range []string{".git/hooks/pre-commit", ".git/config", "sub/.git/x", ".GIT/config", ".Git", ".gi‌t", ".g‌it/x", "git~1/x", ".git ", ".git."} {
		if out, isErr := e.wr.Run(context.Background(), wr(p, "evil")); !isErr {
			t.Fatalf("write %q accepted: %s", p, out)
		}
	}
	if mustFile(t, filepath.Join(e.wt, ".git", "config")) != "c" {
		t.Fatal(".git/config changed")
	}
	if _, err := os.Stat(filepath.Join(e.wt, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("hook created")
	}
	if !tools.GitName(".g‌it") || tools.GitName(".github") || tools.GitName("git") {
		t.Fatal("gitName rule")
	}
}

func TestEditWriteLimits(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", "a")
	big := strings.Repeat("x", tools.MaxFileBytes+1)
	if _, isErr := e.wr.Run(context.Background(), wr("n/m/big.txt", big)); !isErr {
		t.Fatal("over-limit content accepted")
	}
	if _, err := os.Stat(filepath.Join(e.wt, "n")); err == nil {
		t.Fatal("directories made by a refused write remain")
	}
	// a failure after mkdir removes what the call made
	if _, isErr := e.wr.Run(context.Background(), wr("n/m/f.txt", "x", "expect_sha256", strings.Repeat("1", 64))); !isErr {
		t.Fatal("mismatched expect accepted")
	}
	if _, err := os.Stat(filepath.Join(e.wt, "n")); err == nil {
		t.Fatal("directories made by a failed write remain")
	}
	e.put(t, "r.txt", strings.Repeat("y", tools.MaxFileBytes-2))
	if _, isErr := e.ed.Run(context.Background(), edits("r.txt", "yy", "zzzzzz")); !isErr {
		t.Fatal("result over the limit accepted")
	}
	var many []string
	for i := 0; i < 65; i++ {
		many = append(many, fmt.Sprint("o", i), "n")
	}
	if _, isErr := e.ed.Run(context.Background(), edits("f.txt", many...)); !isErr {
		t.Fatal("65 edits accepted")
	}
	if _, isErr := e.ed.Run(context.Background(), edits("f\x00.txt", "a", "b")); !isErr {
		t.Fatal("NUL accepted")
	}
	if mustFile(t, p) != "a" {
		t.Fatal("file changed")
	}
}

func TestWriteCreatesKeepsModeAndLeavesNoTemp(t *testing.T) {
	e := newEditEnv(t)
	if out, isErr := e.wr.Run(context.Background(), wr("a/b/new.txt", "hello\n")); isErr {
		t.Fatal(out)
	}
	if mustFile(t, filepath.Join(e.wt, "a/b/new.txt")) != "hello\n" {
		t.Fatal("content")
	}
	p := e.put(t, "run.sh", "#!/bin/sh\n")
	os.Chmod(p, 0o755)
	if out, isErr := e.wr.Run(context.Background(), wr("run.sh", "#!/bin/sh\necho hi\n")); isErr {
		t.Fatal(out)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
	e.wr.Run(context.Background(), wr("run.sh", "y", "expect_sha256", strings.Repeat("2", 64))) // forced error
	var tmps []string
	filepath.Walk(e.wt, func(p string, _ os.FileInfo, _ error) error {
		if strings.Contains(filepath.Base(p), ".fabric-") {
			tmps = append(tmps, p)
		}
		return nil
	})
	if len(tmps) > 0 {
		t.Fatalf("temp files remain: %v", tmps)
	}
}

func TestWriteConcurrentReaderSeesOldOrNew(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", strings.Repeat("old\n", 1000))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	bad := make(chan string, 1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, _ := os.ReadFile(p)
			s := string(b)
			if s != strings.Repeat("old\n", 1000) && s != strings.Repeat("new\n", 1000) {
				select {
				case bad <- s[:min(20, len(s))]:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		c := "new\n"
		if i%2 == 1 {
			c = "old\n"
		}
		e.wr.Run(context.Background(), wr("f.txt", strings.Repeat(c, 1000)))
	}
	close(stop)
	wg.Wait()
	select {
	case s := <-bad:
		t.Fatalf("a reader saw a torn file: %q", s)
	default:
	}
}

func TestEditLostUpdateRefused(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", "alpha beta")
	var ran atomic.Bool
	var renames atomic.Int32 // temp files written up to the rename window
	restore := tools.SetHooks(func() {
		if ran.CompareAndSwap(false, true) { // the second writer runs completely between A's read and A's commit
			if out, isErr := e.ed.Run(context.Background(), edits("f.txt", "beta", "BETA")); isErr {
				t.Errorf("second writer: %s", out)
			}
		}
	}, func() { renames.Add(1) }, nil)
	defer restore()
	out, isErr := e.ed.Run(context.Background(), edits("f.txt", "alpha", "ALPHA"))
	if !isErr || !strings.Contains(out, "file changed") {
		t.Fatalf("lost update not refused: %v %s", isErr, out)
	}
	if got := mustFile(t, p); got != "alpha BETA" {
		t.Fatalf("first edit must survive, got %q", got)
	}
	// the stale writer is refused under the lock, before any temp file is written:
	// only the second writer (which committed) reached the rename window
	if n := renames.Load(); n != 1 {
		t.Fatalf("%d writers reached the rename window, want 1 (the stale one must be refused earlier)", n)
	}
}

func TestEditParallelDifferentFiles(t *testing.T) {
	e := newEditEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		e.put(t, name, "v0")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				if out, isErr := e.ed.Run(context.Background(), edits(name, fmt.Sprintf("v%d", k), fmt.Sprintf("v%d", k+1))); isErr {
					t.Errorf("%s: %s", name, out)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestWritePreimageUntrackedKeepsCopy(t *testing.T) {
	e := newEditEnv(t)
	e.gitInit(t)
	e.put(t, "tracked.txt", "T0")
	e.git(t, "add", "tracked.txt")
	e.git(t, "commit", "-q", "-m", "c0")
	e.put(t, "untracked.txt", "U0")
	out, isErr := e.wr.Run(context.Background(), wr("untracked.txt", "U1"))
	if isErr || !strings.Contains(out, "preimage_copy: pre-") {
		t.Fatalf("untracked overwrite keeps no copy: %s", out)
	}
	name := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "preimage_copy: ", 2)[1], "\n", 2)[0])
	if got := mustFile(t, filepath.Join(e.keep, name)); got != "U0" {
		t.Fatalf("copy holds %q", got)
	}
	out, isErr = e.wr.Run(context.Background(), wr("tracked.txt", "T1"))
	if isErr || strings.Contains(out, "preimage_copy") || !strings.Contains(out, "preimage_git_blob: ") {
		t.Fatalf("a clean tracked file is restored from git, not copied: %s", out)
	}
}

func (e *editEnv) git(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = e.wt
	o, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, o)
	}
	return string(o)
}

func copyName(t *testing.T, out string) string {
	t.Helper()
	parts := strings.SplitN(out, "preimage_copy: ", 2)
	if len(parts) != 2 {
		t.Fatalf("no pre-image copy: %s", out)
	}
	return strings.TrimSpace(strings.SplitN(parts[1], "\n", 2)[0])
}

// F1 (goodqa#15361 r1b): a tracked file with uncommitted changes is not
// restorable from git, so the edit keeps a copy, and the copy restores the
// dirty bytes.
func TestEditPreimageDirtyTrackedKeepsCopy(t *testing.T) {
	e := newEditEnv(t)
	e.gitInit(t)
	p := e.put(t, "d.txt", "committed alpha\n")
	e.git(t, "add", "d.txt")
	e.git(t, "commit", "-q", "-m", "c0")
	e.put(t, "d.txt", "dirty alpha, uncommitted work\n")
	out, isErr := e.ed.Run(context.Background(), edits("d.txt", "dirty alpha", "edited"))
	if isErr {
		t.Fatal(out)
	}
	name := copyName(t, out)
	saved := mustFile(t, filepath.Join(e.keep, name))
	if saved != "dirty alpha, uncommitted work\n" {
		t.Fatalf("the copy holds %q, not the dirty bytes", saved)
	}
	// restore from the copy: the uncommitted work comes back
	if err := os.WriteFile(p, []byte(saved), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustFile(t, p); got != "dirty alpha, uncommitted work\n" {
		t.Fatalf("restore gave %q", got)
	}
}

// F1: a staged change (index equals the file, HEAD does not) and a file that
// is only staged (no HEAD commit) both keep a copy; a file equal to both the
// index and HEAD does not, and names its blob.
func TestWritePreimageStagedAndCleanTracked(t *testing.T) {
	e := newEditEnv(t)
	e.gitInit(t)
	e.put(t, "new.txt", "N0")
	e.git(t, "add", "new.txt") // staged, no commit yet
	if out, isErr := e.wr.Run(context.Background(), wr("new.txt", "N1")); isErr || copyName(t, out) == "" {
		t.Fatalf("staged-only file: %s", out)
	}
	e.put(t, "s.txt", "S0")
	e.git(t, "add", "s.txt", "new.txt")
	e.git(t, "commit", "-q", "-m", "c0")
	e.put(t, "s.txt", "S1 staged")
	e.git(t, "add", "s.txt") // index == disk, HEAD differs
	out, isErr := e.wr.Run(context.Background(), wr("s.txt", "S2"))
	if isErr || mustFile(t, filepath.Join(e.keep, copyName(t, out))) != "S1 staged" {
		t.Fatalf("staged change: %s", out)
	}
	e.git(t, "add", "s.txt")
	e.git(t, "commit", "-q", "-m", "c1")
	out, isErr = e.wr.Run(context.Background(), wr("s.txt", "S3"))
	blob := strings.TrimSpace(e.git(t, "rev-parse", "HEAD:s.txt"))
	if isErr || strings.Contains(out, "preimage_copy") || !strings.Contains(out, "preimage_git_blob: "+blob) {
		t.Fatalf("clean tracked file: %s", out)
	}
}

func newKeepEnv(t *testing.T, maxFiles int, maxBytes int64) *editEnv {
	t.Helper()
	e := newEditEnv(t)
	dir, err := tools.OpenKeepDir(filepath.Join(filepath.Dir(e.keep), "small"), "pre-", maxFiles, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.Close)
	e.keep = dir.Path()
	e.pre = &tools.PreimageKeeper{Dir: dir}
	e.ed, e.wr = tools.NewEdit(e.rd, e.pin, e.pre), tools.NewWrite(e.rd, e.pin, e.pre)
	return e
}

// F2: at the file limit the call is refused by name, the target is
// unchanged, and every earlier copy is still there (nothing is evicted).
func TestPreimageStoreFullRefusesNoEviction(t *testing.T) {
	e := newKeepEnv(t, 2, 1<<20)
	var names []string
	for _, f := range []string{"a", "b"} {
		e.put(t, f+".txt", f+"0")
		out, isErr := e.wr.Run(context.Background(), wr(f+".txt", f+"1"))
		if isErr {
			t.Fatal(out)
		}
		names = append(names, copyName(t, out))
	}
	p := e.put(t, "c.txt", "c0")
	out, isErr := e.ed.Run(context.Background(), edits("c.txt", "c0", "c1"))
	if !isErr || !strings.Contains(out, "pre-image store is full") || !strings.Contains(out, "unchanged") {
		t.Fatalf("a full store must refuse the call by name: %v %s", isErr, out)
	}
	if got := mustFile(t, p); got != "c0" {
		t.Fatalf("refused call changed the file: %q", got)
	}
	for i, n := range names {
		if got := mustFile(t, filepath.Join(e.keep, n)); got != []string{"a0", "b0"}[i] {
			t.Fatalf("copy %s evicted or changed: %q", n, got)
		}
	}
	// a new file needs no copy and still works
	if out, isErr := e.wr.Run(context.Background(), wr("fresh.txt", "x")); isErr {
		t.Fatal(out)
	}
}

// F2: the byte limit refuses too, and a refused or aborted copy releases
// nothing it did not hold.
func TestPreimageStoreByteLimit(t *testing.T) {
	e := newKeepEnv(t, 100, 10)
	e.put(t, "a.txt", "12345678")
	if out, isErr := e.wr.Run(context.Background(), wr("a.txt", "x")); isErr {
		t.Fatal(out)
	}
	e.put(t, "b.txt", "12345678")
	out, isErr := e.wr.Run(context.Background(), wr("b.txt", "x"))
	if !isErr || !strings.Contains(out, "pre-image store is full") {
		t.Fatalf("byte limit: %v %s", isErr, out)
	}
	if got := mustFile(t, filepath.Join(e.wt, "b.txt")); got != "12345678" {
		t.Fatalf("refused write changed the file: %q", got)
	}
}

// F2: call journals (j-*) and the gitdir record left by an earlier run do
// not count toward the copy limit after a restart; earlier copies do.
func TestPreimageJournalsNotCounted(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "keep")
	d, err := tools.OpenKeepDir(dir, "pre-", 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("j-toolu_%02d", i)), []byte(`{"path":"x","pre":"a","post":"b"}`), 0o600)
	}
	os.WriteFile(filepath.Join(dir, "gitref"), []byte(`{"v":1}`), 0o600)
	d.Close()
	d, err = tools.OpenKeepDir(dir, "pre-", 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := 0; i < 2; i++ {
		f, _, err := d.CreateReserved("pre-", 1)
		if err != nil {
			t.Fatalf("copy %d refused although only journals are present: %v", i, err)
		}
		f.Close()
	}
	if _, _, err := d.CreateReserved("pre-", 1); !errors.Is(err, tools.ErrPreimageStoreFull) {
		t.Fatalf("third copy: %v", err)
	}
	d.Close()
	// the two copies survive a restart and still count
	d2, err := tools.OpenKeepDir(dir, "pre-", 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, _, err := d2.CreateReserved("pre-", 1); !errors.Is(err, tools.ErrPreimageStoreFull) {
		t.Fatalf("after restart: %v", err)
	}
}

// F4: the gitdir is recorded once per attempt. A resume uses the record even
// when the child repointed the worktree's gitfile at a planted repository
// where the file is tracked and clean; a recorded gitdir that was replaced is
// refused; an attempt that started without git stays without git.
func TestAttemptGitRefResume(t *testing.T) {
	e := newEditEnv(t)
	gd := filepath.Join(filepath.Dir(e.wt), "gd")
	c := exec.Command("git", "init", "-q", "--separate-git-dir="+gd)
	c.Dir = e.wt
	if o, err := c.CombinedOutput(); err != nil {
		t.Fatal(string(o))
	}
	g1, err := tools.AttemptGitRef(e.pre.Dir, e.wt)
	if err != nil || g1 == nil || g1.GitDir != gd {
		t.Fatalf("first record: %+v %v", g1, err)
	}
	e.put(t, "x.txt", "X0") // untracked in gd
	planted := filepath.Join(filepath.Dir(e.wt), "planted")
	os.MkdirAll(planted, 0o755)
	os.WriteFile(filepath.Join(planted, "x.txt"), []byte("X0"), 0o644)
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "x.txt"}, {"commit", "-q", "-m", "p"}} {
		c := exec.Command("git", a...)
		c.Dir = planted
		if o, err := c.CombinedOutput(); err != nil {
			t.Fatal(string(o))
		}
	}
	os.WriteFile(filepath.Join(e.wt, ".git"), []byte("gitdir: "+filepath.Join(planted, ".git")+"\n"), 0o644)
	// resume: a new runner process for the same attempt
	g2, err := tools.AttemptGitRef(e.pre.Dir, e.wt)
	if err != nil || g2 == nil || g2.GitDir != gd {
		t.Fatalf("resume trusted the rewritten .git: %+v %v", g2, err)
	}
	e.pre.Git = g2
	if out, isErr := e.wr.Run(context.Background(), wr("x.txt", "X1")); isErr || copyName(t, out) == "" {
		t.Fatalf("resume: %s", out)
	}
	// the recorded gitdir itself replaced by another directory (moved aside
	// first, so ext4 cannot hand the new directory the freed inode)
	if err := os.Rename(gd, gd+".old"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(gd, 0o755)
	if _, err := tools.AttemptGitRef(e.pre.Dir, e.wt); !errors.Is(err, tools.ErrGitDirChanged) {
		t.Fatalf("a replaced recorded gitdir must be refused: %v", err)
	}
	// an attempt that started without git: a later .git is never adopted
	e2 := newEditEnv(t)
	if g, err := tools.AttemptGitRef(e2.pre.Dir, e2.wt); g != nil || err != nil {
		t.Fatalf("no git at start: %+v %v", g, err)
	}
	e2.gitInit(t)
	if g, err := tools.AttemptGitRef(e2.pre.Dir, e2.wt); g != nil || err != nil {
		t.Fatalf("resume adopted a .git created after the start: %+v %v", g, err)
	}
}

func TestWritePreimagePlantedGitdir(t *testing.T) {
	e := newEditEnv(t)
	e.gitInit(t) // the gitdir recorded at attempt start
	e.put(t, "x.txt", "X0")
	// the child plants a repository where x.txt is tracked and repoints .git at it
	planted := filepath.Join(filepath.Dir(e.wt), "planted")
	os.MkdirAll(planted, 0o755)
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = planted
		c.CombinedOutput()
	}
	os.WriteFile(filepath.Join(planted, "x.txt"), []byte("X0"), 0o644)
	// committed and clean there, so only the gitdir check can force the copy
	for _, a := range [][]string{{"add", "x.txt"}, {"commit", "-q", "-m", "p"}} {
		c := exec.Command("git", a...)
		c.Dir = planted
		if o, err := c.CombinedOutput(); err != nil {
			t.Fatal(string(o))
		}
	}
	os.RemoveAll(filepath.Join(e.wt, ".git"))
	os.WriteFile(filepath.Join(e.wt, ".git"), []byte("gitdir: "+filepath.Join(planted, ".git")+"\n"), 0o644)
	out, isErr := e.wr.Run(context.Background(), wr("x.txt", "X1"))
	if isErr || !strings.Contains(out, "preimage_copy: pre-") {
		t.Fatalf("a planted gitdir suppressed the pre-image copy: %s", out)
	}
}

func TestWritePreimageRiskyConfigKeepsCopy(t *testing.T) {
	e := newEditEnv(t)
	e.gitInit(t)
	e.put(t, "t.txt", "T0")
	// committed and clean, so only the risky configuration can force the copy
	e.git(t, "add", "t.txt")
	e.git(t, "commit", "-q", "-m", "c0")
	e.git(t, "config", "url.https://x/.insteadOf", "y:")
	out, isErr := e.wr.Run(context.Background(), wr("t.txt", "T1"))
	if isErr || !strings.Contains(out, "preimage_copy: pre-") {
		t.Fatalf("a refused git query must keep the copy: %s", out)
	}
}

func TestEditInterruptedNoteAndTempCleanup(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", "before")
	var landed string
	// crash after the rename: the call never reports, the temp is gone, the note shows digests
	restore := tools.SetHooks(nil, nil, func() { landed = "yes" })
	ctx := agent.WithToolCallContext(context.Background(), agent.ToolCallContext{ID: "toolu_0123456789ABCDEFGH", MessageIndex: 1})
	in := edits("f.txt", "before", "after")
	if out, isErr := e.ed.Run(ctx, in); isErr {
		t.Fatal(out)
	}
	restore()
	if landed == "" {
		t.Fatal("hook not run")
	}
	// plant a leftover temp of that call, as a crash before the rename would
	tmp := filepath.Join(e.wt, ".fabric-"+tagTail("toolu_0123456789ABCDEFGH")+".tmp")
	os.WriteFile(tmp, []byte("partial"), 0o644)
	note := e.ed.InterruptedNote("toolu_0123456789ABCDEFGH", in)
	for _, s := range []string{sha([]byte("after")), sha([]byte("before"))} {
		if !strings.Contains(note, s) {
			t.Fatalf("note lacks %s: %s", s, note)
		}
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("leftover temp not removed")
	}
	if mustFile(t, p) != "after" {
		t.Fatal("edited twice or lost")
	}
}

func tagTail(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	return s[len(s)-8:]
}

// The second pre-image check, right before renameat: a change between the
// temp write and the rename is refused and nothing is left behind.
func TestEditChangedBeforeRename(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "f.txt", "alpha beta")
	restore := tools.SetHooks(nil, func() { os.WriteFile(p, []byte("alpha BETA!"), 0o644) }, nil)
	defer restore()
	out, isErr := e.ed.Run(context.Background(), edits("f.txt", "alpha", "ALPHA"))
	if !isErr || !strings.Contains(out, "file changed") {
		t.Fatalf("a change before the rename was not refused: %v %s", isErr, out)
	}
	if got := mustFile(t, p); got != "alpha BETA!" {
		t.Fatalf("the other writer's bytes were overwritten: %q", got)
	}
	if ents, _ := filepath.Glob(filepath.Join(e.wt, ".fabric-*")); len(ents) != 0 {
		t.Fatalf("temp left behind: %v", ents)
	}
}
