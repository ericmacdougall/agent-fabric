package pin

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// goodqa#15966 AC-21 tests. A non-root test run cannot create root-owned
// files, so the stat seam presents every path as root-owned, directories as
// 0755, and files with their real mode, except where a case overrides it.

type fakeInfo struct {
	fs.FileInfo
	uid  uint32
	mode fs.FileMode
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) Sys() any {
	st := *f.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid = f.uid
	return &st
}

type pinEnv struct {
	dir      string
	bin      string
	sha      string
	uid      map[string]uint32      // path -> owner override
	mode     map[string]fs.FileMode // path -> mode override (perm bits)
	realDirs bool
}

func newPinEnv(t *testing.T) *pinEnv {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &pinEnv{dir: dir, uid: map[string]uint32{}, mode: map[string]fs.FileMode{}}
	e.bin = filepath.Join(dir, "claude")
	e.write(t, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("native claude build one")...))
	old := Seam
	t.Cleanup(func() { Seam = old })
	Seam.lstat = func(p string) (fs.FileInfo, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fi, nil // symlinks stay visible as symlinks
		}
		m := fi.Mode()
		if fi.IsDir() {
			m = fs.ModeDir | 0o755
		}
		if o, ok := e.mode[p]; ok {
			m = m&^fs.ModePerm | o
		}
		u := uint32(0)
		if o, ok := e.uid[p]; ok {
			u = o
		}
		return fakeInfo{fi, u, m}, nil
	}
	return e
}

func (e *pinEnv) write(t *testing.T, b []byte) {
	t.Helper()
	tmp := e.bin + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, e.bin); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	e.sha = hex.EncodeToString(s[:])
}

func mustPin(t *testing.T, e *pinEnv) Wired {
	t.Helper()
	w, err := Wire(e.bin, e.sha)
	if err != nil {
		t.Fatalf("a correctly pinned root-owned binary was refused: %v", err)
	}
	return w
}

func wantRefusedAtStart(t *testing.T, e *pinEnv, want string) {
	t.Helper()
	_, err := Wire(e.bin, e.sha)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal containing %q, got %v", want, err)
	}
}

func TestPinAcceptsTheGoodBinaryAndFakeStartsIgnoreIt(t *testing.T) {
	e := newPinEnv(t)
	w := mustPin(t, e)
	if got := w.Resolve(e.bin); got != e.bin {
		t.Fatalf("Resolve = %q, want the pinned path", got)
	}
	if err := w.PreExec(e.bin); err != nil {
		t.Fatalf("pre-exec check of the pinned binary: %v", err)
	}
}

func TestPinRefusesAWrongSHA(t *testing.T) {
	e := newPinEnv(t)
	wrong := strings.Repeat("0", 64)
	_, err := Wire(e.bin, wrong)
	if err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("a wrong sha must refuse the start, got %v", err)
	}
}

func TestPinRefusesABinarySwappedBetweenLaunches(t *testing.T) {
	e := newPinEnv(t)
	w := mustPin(t, e)
	if w.Resolve(e.bin) == "" {
		t.Fatal("first launch refused")
	}
	// Swapped after launch 1 (an updater or a worker-side replacement).
	e.write(t, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("a different build")...))
	if got := w.Resolve(e.bin); got != "" {
		t.Fatalf("launch 2 resolved %q after the binary was swapped", got)
	}
	// Swapped in the window between Resolve and exec: the pre-exec check.
	e.write(t, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("native claude build one")...))
	s := sha256.Sum256(append([]byte{0x7f, 'E', 'L', 'F'}, []byte("native claude build one")...))
	e.sha = hex.EncodeToString(s[:])
	w = mustPin(t, e)
	path := w.Resolve(e.bin)
	if path == "" {
		t.Fatal("restored binary refused")
	}
	e.write(t, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("swapped after resolve")...))
	if err := w.PreExec(path); err == nil {
		t.Fatal("pre-exec accepted a binary swapped after Resolve")
	}
	// A different, otherwise valid copy of the same bytes is not the pinned path.
	copyPath := filepath.Join(e.dir, "claude-copy")
	b := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("native claude build one")...)
	if err := os.WriteFile(copyPath, b, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(t, b) // restore the pinned file so only the path differs
	if err := w.PreExec(copyPath); err == nil {
		t.Fatal("pre-exec accepted an argv[0] other than the pinned path")
	}
}

func TestPinRefusesAWritableParent(t *testing.T) {
	for _, perm := range []fs.FileMode{0o775, 0o757, 0o777, 0o722} {
		for _, level := range []string{"direct", "grandparent"} {
			e := newPinEnv(t)
			sub := filepath.Join(e.dir, "a", "b")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(sub, "claude")
			if err := os.Rename(e.bin, bin); err != nil {
				t.Fatal(err)
			}
			e.bin = bin
			target := sub
			if level == "grandparent" {
				target = filepath.Join(e.dir, "a")
			}
			w := mustPin(t, e) // good until the parent becomes writable
			e.mode[target] = perm
			wantRefusedAtStart(t, e, "writable by group or others")
			// And at every launch, after a start that was fine.
			if got := w.Resolve(e.bin); got != "" {
				t.Fatalf("%v %s: launch resolved with a writable parent", perm, level)
			}
			if err := w.PreExec(e.bin); err == nil {
				t.Fatalf("%v %s: pre-exec accepted a writable parent", perm, level)
			}
		}
	}
}

func TestPinRefusesNonRootOwners(t *testing.T) {
	for _, who := range []string{"binary", "parent", "grandparent"} {
		e := newPinEnv(t)
		path := map[string]string{"binary": e.bin, "parent": e.dir, "grandparent": filepath.Dir(e.dir)}[who]
		e.uid[path] = 1000 // the worker's uid, say
		wantRefusedAtStart(t, e, "not owned by root")
	}
}

func TestPinRefusesAWritableOrSetuidBinary(t *testing.T) {
	for name, m := range map[string]fs.FileMode{"group-writable": 0o775, "world-writable": 0o757, "not executable": 0o644, "setuid": 0o755 | fs.ModeSetuid} {
		e := newPinEnv(t)
		e.mode[e.bin] = m
		if m&fs.ModeSetuid != 0 {
			Seam.lstat = func(p string) (fs.FileInfo, error) {
				fi, _ := os.Lstat(p)
				mode := fi.Mode()
				if p == e.bin {
					mode |= fs.ModeSetuid
				} else if fi.IsDir() {
					mode = fs.ModeDir | 0o755
				}
				return fakeInfo{fi, 0, mode}, nil
			}
		}
		if _, err := Wire(e.bin, e.sha); err == nil {
			t.Fatalf("%s binary was accepted", name)
		}
	}
}

func TestPinRefusesASymlinkedBinary(t *testing.T) {
	e := newPinEnv(t)
	link := filepath.Join(e.dir, "claude-link")
	if err := os.Symlink(e.bin, link); err != nil {
		t.Fatal(err)
	}
	_, err := Wire(link, e.sha)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked binary must be refused, got %v", err)
	}
	// A symlinked parent directory is refused too.
	real := filepath.Join(e.dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.bin, filepath.Join(real, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(e.dir, "viadir")); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(e.dir, "viadir", "claude")
	if _, err := Wire(via, e.sha); err == nil {
		t.Fatal("a binary reached through a symlinked directory was accepted")
	}
	// A pinned binary replaced by a symlink between launches is refused.
	e2 := newPinEnv(t)
	w2 := mustPin(t, e2)
	other := filepath.Join(e2.dir, "other")
	if err := os.Rename(e2.bin, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, e2.bin); err != nil {
		t.Fatal(err)
	}
	if got := w2.Resolve(e2.bin); got != "" {
		t.Fatalf("launch followed a symlink swapped in after the start: %q", got)
	}
}

func TestPinRefusesAScriptShim(t *testing.T) {
	e := newPinEnv(t)
	e.write(t, []byte("#!/usr/bin/env node\nconsole.log('npm shim')\n"))
	wantRefusedAtStart(t, e, "native single-file")
}
