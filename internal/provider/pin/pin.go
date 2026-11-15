// Package pin is the goodqa#15966 AC-21 provider pin.
package pin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
)

// goodqa#15966 AC-21: a real provider is the native single-file claude build,
// pinned by --provider-sha256 and installed in a root-owned directory whose
// every parent is writable only by root. fabd checks the pin at start and at
// every launch, and again immediately before exec (claude.Launch.PreExec),
// then execs by path. Root-only parents leave no uid that can rename a
// component between that last check and the exec, which removes the
// hash-then-exec window without a sealed copy (the pinned binary is
// hundreds of MiB and signed or self-contained).

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pinSeam is the filesystem view the pin checks use; tests replace it to
// present ownership a non-root test run cannot create.
type SeamFuncs struct {
	lstat func(string) (fs.FileInfo, error)
	open  func(string) (*os.File, error)
}

var Seam = SeamFuncs{
	lstat: os.Lstat,
	open:  func(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0) },
}

func pinUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func pinIdentity(fi fs.FileInfo) (dev, ino uint64, ok bool) {
	st, k := fi.Sys().(*syscall.Stat_t)
	if !k {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true //nolint:unconvert // width differs by platform
}

// Verify is the whole AC-21 check, run at start, at every
// launch and immediately before exec. It returns nil only when path is an
// absolute, symlink-free path to a regular ELF or Mach-O file (not a script
// shim, not setuid) owned by root, not group- or world-writable, every
// parent directory is root-owned and not group- or world-writable, and the
// bytes read through the checked descriptor hash to want.
func Verify(path, want string) error {
	if !sha256Hex.MatchString(want) {
		return fmt.Errorf("provider pin %q is not a lower-case sha256", want)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("provider %q is not a clean absolute path", path)
	}
	s := Seam
	fi, err := s.lstat(path)
	if err != nil {
		return fmt.Errorf("provider %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provider %s is a symlink; the pinned binary must be the file itself", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("provider %s is not a regular file", path)
	}
	if uid, ok := pinUID(fi); !ok || uid != 0 {
		return fmt.Errorf("provider %s is not owned by root", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("provider %s is writable by group or others (mode %v)", path, fi.Mode())
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("provider %s must be executable and not setuid or setgid (mode %v)", path, fi.Mode())
	}
	for d := filepath.Dir(path); ; d = filepath.Dir(d) {
		di, err := s.lstat(d)
		if err != nil {
			return fmt.Errorf("provider parent %s: %w", d, err)
		}
		if di.Mode()&os.ModeSymlink != 0 || !di.IsDir() {
			return fmt.Errorf("provider parent %s is not a plain directory", d)
		}
		if uid, ok := pinUID(di); !ok || uid != 0 {
			return fmt.Errorf("provider parent %s is not owned by root", d)
		}
		if di.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("provider parent %s is writable by group or others (mode %v), so another uid could replace the binary", d, di.Mode())
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	f, err := s.open(path)
	if err != nil {
		return fmt.Errorf("provider %s: %w", path, err)
	}
	defer f.Close()
	ffi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("provider %s: %w", path, err)
	}
	// The descriptor must be the file that was checked, not a swap between
	// the lstat and the open.
	d1, i1, ok1 := pinIdentity(fi)
	d2, i2, ok2 := pinIdentity(ffi)
	if !ok1 || !ok2 || d1 != d2 || i1 != i2 {
		return fmt.Errorf("provider %s changed while it was checked", path)
	}
	h := sha256.New()
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil {
		return fmt.Errorf("provider %s is too short to be a native binary", path)
	}
	h.Write(head[:n])
	if !nativeMagic(head) {
		return fmt.Errorf("provider %s is not a native single-file binary (a script or shim is refused)", path)
	}
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("provider %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("provider %s has sha256 %s, not the pinned %s", path, got, want)
	}
	return nil
}

func nativeMagic(b []byte) bool {
	switch {
	case b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F':
		return true
	case b[0] == 0xcf && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe, // Mach-O 64 LE
		b[0] == 0xca && b[1] == 0xfe && b[2] == 0xba && b[3] == 0xbe: // universal
		return true
	}
	return false
}

// Wired is a pinned provider: the path every launch execs and the two
// per-launch checks.
type Wired struct {
	Path    string
	Resolve func(name string) string
	PreExec func(argv0 string) error
}

// Lookup resolves name like exec.LookPath but never follows symlinks.
func Lookup(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	return abs
}

// Wire checks the pin now and returns the per-launch Resolve (every launch
// re-checks sha, owner and mode and returns the path to exec) and PreExec
// (the last check, immediately before the host execs argv[0]).
func Wire(binary, want string) (Wired, error) {
	pinned := Lookup(binary)
	if pinned == "" {
		return Wired{}, fmt.Errorf("provider %q cannot be resolved to a file", binary)
	}
	if err := Verify(pinned, want); err != nil {
		return Wired{}, err
	}
	return Wired{
		Path: pinned,
		Resolve: func(name string) string {
			p := Lookup(name)
			if p == "" {
				return ""
			}
			if err := Verify(p, want); err != nil {
				fmt.Fprintf(os.Stderr, "fab: refusing launch (goodqa#15966 AC-21): %v\n", err)
				return ""
			}
			return p
		},
		PreExec: func(argv0 string) error {
			if argv0 != pinned {
				return fmt.Errorf("launch would exec %s, not the pinned %s", argv0, pinned)
			}
			return Verify(argv0, want)
		},
	}, nil
}
