package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"fabric/internal/dispatch"
	"fabric/internal/host"
)

// realProviderRefused is the goodqa#15976 AC-2 refusal: a real provider on a
// host profile that has not shown a worker boundary. It exits 3 (a refusal;
// nothing was started).
type realProviderRefused struct{ reason string }

func (e realProviderRefused) Error() string {
	return "refusing to run a real provider (goodqa#15976): " + e.reason +
		"; only a pinned fake (the stdin fake or the recorded fakeclaude, by sha256) may run here"
}

// providerIsPinnedFake hashes the resolved provider file. A file that cannot
// be hashed is real: identity comes from content, never from a name or a
// flag such as --trust-provider.
func providerIsPinnedFake(path string) bool {
	sha, err := dispatch.FileSHA256(path)
	return err == nil && dispatch.PinnedFake(sha)
}

// daemonProviderGate decides whether fabd may start with this provider
// (goodqa#15976 AC-2, AC-3). A provider is real unless its resolved file
// hashes to a pinned fake. --trust-provider is honoured only for a pinned
// fake and is refused otherwise, even with --no-dispatch (it would skip the
// negative controls). --no-dispatch starts nothing and is otherwise never
// refused. A real provider needs a profile that passes realProviderGate.
// fakeOnly reports that the start rests on the provider being a pinned
// fake: every launch must then re-check the file it resolves, because an
// update or a repointed symlink could swap in a real binary after the start.
func daemonProviderGate(noDispatch, trustProvider bool, providerPath string, p host.Profile) (fakeOnly bool, err error) {
	fake := providerIsPinnedFake(providerPath)
	if trustProvider && !fake {
		return false, realProviderRefused{fmt.Sprintf("--trust-provider is honoured only for a pinned fake, and %s does not hash to one", providerPath)}
	}
	if noDispatch || trustProvider {
		return trustProvider, nil
	}
	gateErr := realProviderGate(p)
	if gateErr == nil {
		return false, nil
	}
	if fake {
		return true, nil
	}
	return false, gateErr
}

// gateDaemonProvider applies daemonProviderGate to fabd's provider and, for
// a fake-only start, makes its launch re-check the resolved file at every
// launch (AC-3), so a binary swapped in after the start is refused. real
// reports that fabd may dispatch a real provider.
func gateDaemonProvider(dp *daemonProvider, noDispatch, trustProvider bool, p host.Profile) (real bool, err error) {
	fakeOnly, err := daemonProviderGate(noDispatch, trustProvider, dp.Path, p)
	if err != nil {
		return false, err
	}
	if fakeOnly {
		dp.Launch.Resolve = pinnedFakeResolve(dp.Launch.Resolve, dp.SealRoot)
	}
	return !noDispatch && !fakeOnly, nil
}

// daemonExitCleanup (goodqa#15976 AC-5): verified descendant cleanup after
// every worker exit is on by default whenever fabd may run a real provider;
// FABRIC_WORKER_EXIT_CLEANUP=1 still turns it on for the fakes. (It stays
// opt-in for the fakes because of the Darwin PGID-reuse window; macOS never
// runs a real provider.)
func daemonExitCleanup(real bool) bool {
	return real || os.Getenv("FABRIC_WORKER_EXIT_CLEANUP") == "1"
}

// nativeRunnerGate (goodqa#15976 AC-2): the native anthropic-api runner
// calls a real model, and M1 builds keep it off on every host. Only the
// existing test-only --unconfined-fake-host mode runs.
func nativeRunnerGate() error {
	return realProviderRefused{"the native anthropic-api runner is off in M1 builds (only --unconfined-fake-host, a test mode, runs)"}
}

// harnessProviderGate (goodqa#15976 AC-2) covers `fab harness run`: the
// claude-code adapter runs its binary unconfined, so only a pinned fake
// runs; a codex model turn (--codex-run) is a real provider and is refused;
// the codex preflight runs no model turn; native is nativeRunnerGate.
func harnessProviderGate(adapter, claudeBin string, codexRun, unconfinedFake bool) error {
	switch adapter {
	case "native":
		if !unconfinedFake {
			return nativeRunnerGate()
		}
	case "claude-code":
		path, err := exec.LookPath(claudeBin)
		if err != nil || !(providerIsPinnedFake(path) || harnessTestFake(path)) {
			return realProviderRefused{fmt.Sprintf("the claude-code harness adapter runs unconfined, and %q is not a pinned fake", claudeBin)}
		}
	case "codex":
		if codexRun {
			return realProviderRefused{"a codex model turn (--codex-run) is a real provider"}
		}
	}
	return nil
}

// demoProviderGate (goodqa#15976 AC-2): `fab harness demo` always drives a
// real native client, so M1 builds refuse it.
func demoProviderGate(provider string) error {
	return realProviderRefused{fmt.Sprintf("fab harness demo runs the real %s client", provider)}
}

// harnessTestPins holds the sha256 of the throwaway claude fakes the
// harness tests write (their behaviour is the subject of those tests). Only
// test code in this package sets it; it is nil in a built binary.
var harnessTestPins map[string]bool

func harnessTestFake(path string) bool {
	if harnessTestPins == nil {
		return false
	}
	sha, err := dispatch.FileSHA256(path)
	return err == nil && harnessTestPins[sha]
}

// pinnedFakeResolve wraps a launch's Resolve so each launch runs only a
// pinned fake: any other resolved file refuses that launch ("" is Resolve's
// refusal; claude.Argv never falls back to the name). The launch execs a
// sealed copy, never the resolved file: verification and execution use the
// same bytes, so replacing the original after the check changes nothing
// (goodqa#15976 review F1).
func pinnedFakeResolve(resolve func(string) string, sealRoot string) func(string) string {
	return func(name string) string {
		p := resolve(name)
		if p == "" {
			return ""
		}
		sealed, err := sealPinnedFake(p, sealRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fab: refusing launch (goodqa#15976): %v\n", err)
			return ""
		}
		return sealed
	}
}

// sealRootFor is the sealed-copy directory beside fabd's schema file in its
// work root; "" (no sealing possible, every fake launch refused) when the
// schema path is not absolute.
func sealRootFor(schemaPath string) string {
	abs, err := filepath.Abs(schemaPath)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(abs), "pinned-providers")
}

// sealedProviderRO is the sealed-copy directory, a read root of the worker
// sandbox profiles (set by fab daemon before it builds them).
var sealedProviderRO []string

// maxFakeBytes bounds what a seal reads; the reviewed fakes are a few KiB.
const maxFakeBytes = 1 << 20

// readPinned reads path once (no symlink at the last component) and returns
// its bytes only when they hash to want, or to any pinned fake if want is "".
func readPinned(path, want string) ([]byte, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFakeBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxFakeBytes {
		return nil, "", fmt.Errorf("%s is larger than any pinned fake", path)
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	if (want != "" && sha != want) || (want == "" && !dispatch.PinnedFake(sha)) {
		return nil, "", fmt.Errorf("provider %s has sha256 %s, not a pinned fake, and this host has not shown a worker boundary", path, sha)
	}
	return b, sha, nil
}

// Sealed copies have fixed names, independent of what the inputs were
// called: the stdin fake's own lookup (fakeclaude-live first) then always
// finds the sealed recorded fake, and the wrapper can never overwrite its
// delegate (goodqa#15976 delta review F2).
const (
	sealedStdinName    = "fakeclaude-stdin"
	sealedRecordedName = "fakeclaude-live"
)

// sealPinnedFake copies the verified bytes of a pinned fake into
// sealRoot/<sha>/, in directories fabd owns and only fabd can write (0755,
// files 0555), and returns the copy's path. The stdin fake execs the
// recorded fake beside it, so its delegate is verified (RecordedFakeSHA256)
// and sealed into the same directory. The returned path has no symlinks
// and every directory on it is protected (trustedAncestry), so a worker
// running as another uid can read and execute the copies but cannot
// replace them or any directory above them.
func sealPinnedFake(path, sealRoot string) (string, error) {
	if sealRoot == "" || !filepath.IsAbs(sealRoot) {
		return "", errors.New("no sealed provider directory configured")
	}
	b, sha, err := readPinned(path, "")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(sealRoot, sha)
	for _, d := range []string{sealRoot, dir} {
		if err := ownedSealDir(d); err != nil {
			return "", err
		}
	}
	dir, err = trustedAncestry(dir)
	if err != nil {
		return "", err
	}
	name := sealedRecordedName
	if sha == dispatch.StdinFakeSHA256 {
		name = sealedStdinName
		// The stdin fake's own lookup order beside it, never itself.
		var del string
		for _, c := range []string{"fakeclaude-live", "fakeclaude.sh"} {
			if c == filepath.Base(path) {
				continue
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), c)); err == nil {
				del = c
				break
			}
		}
		if del == "" {
			return "", fmt.Errorf("the stdin fake %s has no recorded fake beside it", path)
		}
		db, _, err := readPinned(filepath.Join(filepath.Dir(path), del), dispatch.RecordedFakeSHA256)
		if err != nil {
			return "", fmt.Errorf("stdin fake delegate: %w", err)
		}
		if err := installSealed(dir, sealedRecordedName, db); err != nil {
			return "", err
		}
	}
	if err := installSealed(dir, name, b); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// trustedAncestry resolves dir and requires every directory from it up to
// / to be owned by root or this uid and writable by nobody else, so no
// other uid can rename a component and substitute a tree after the seal is
// verified (goodqa#15976 delta review F1). The one exception is a sticky
// directory (such as /tmp) whose child on the path is owned by this uid:
// the sticky bit stops other uids renaming that child. It returns the
// resolved directory.
func trustedAncestry(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("sealed provider directory: %w", err)
	}
	uid := os.Getuid()
	childUID := -1 // no child below the seal directory itself
	for p := real; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return "", fmt.Errorf("sealed provider ancestor: %w", err)
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || !st.IsDir() {
			return "", fmt.Errorf("sealed provider ancestor %s is not a directory", p)
		}
		owner := int(sys.Uid)
		if owner != 0 && owner != uid {
			return "", fmt.Errorf("sealed provider ancestor %s is owned by uid %d, not root or uid %d", p, owner, uid)
		}
		if st.Mode().Perm()&0o022 != 0 && !(st.Mode()&os.ModeSticky != 0 && childUID == uid) {
			return "", fmt.Errorf("sealed provider ancestor %s is writable by group or others (mode %v), so another uid could replace the sealed copies", p, st.Mode())
		}
		if p == filepath.Dir(p) {
			return real, nil
		}
		childUID = owner
	}
}

// ownedSealDir creates d (0755) if needed and requires a real directory
// owned by this uid and writable by nobody else.
func ownedSealDir(d string) error {
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("sealed provider directory: %w", err)
	}
	st, err := os.Lstat(d)
	if err != nil {
		return fmt.Errorf("sealed provider directory: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("sealed provider directory %s must be a directory owned by uid %d and writable only by it", d, os.Getuid())
	}
	return nil
}

// installSealed writes b to dir/name atomically (temp file, then rename),
// mode 0555, replacing whatever was there.
func installSealed(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, ".seal-*")
	if err != nil {
		return fmt.Errorf("seal %s: %w", name, err)
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Chmod(0o555)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, name))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("seal %s: %w", name, werr)
	}
	return nil
}
