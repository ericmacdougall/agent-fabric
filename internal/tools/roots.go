package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChildWritableRoots lists every directory tree the confined child can write
// (goodqa#15865, red-team R2 and R5): the worktree, the run directory, the
// system temp roots the host profiles open (Darwin: /private/tmp and
// /private/var/folders; Linux: os.TempDir()), /dev, and extra, the profile's
// ExtraRW. Each root is listed both as given and symlink-resolved, so a
// comparison made on resolved paths cannot miss one.
func ChildWritableRoots(worktree, runDir string, extra []string) []string {
	cand := []string{worktree, runDir, "/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp",
		"/var/folders", "/private/var/folders", "/dev", os.TempDir(), os.Getenv("TMPDIR")}
	cand = append(cand, extra...)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, c := range cand {
		if c == "" || !filepath.IsAbs(c) {
			continue
		}
		c = filepath.Clean(c)
		add(c)
		if r, err := filepath.EvalSymlinks(c); err == nil {
			add(r)
		}
	}
	return out
}

// pathWithin reports whether path is root or lies below it, by whole components.
func pathWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// resolveForCompare returns p symlink-resolved. When p does not exist yet, its
// longest existing ancestor is resolved and the rest appended, so a directory
// that is about to be created is judged by where it would really land.
func resolveForCompare(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// UnderAnyRoot reports the first root that path, after symlink resolution,
// lies in. A path that cannot be resolved counts as under every root: the
// answer is fail-closed.
func UnderAnyRoot(path string, roots []string) (string, bool) {
	rp, err := resolveForCompare(path)
	if err != nil {
		return "unresolvable", true
	}
	for _, r := range roots {
		if pathWithin(rp, r) || pathWithin(filepath.Clean(path), r) {
			return r, true
		}
		if rr, err := filepath.EvalSymlinks(r); err == nil && pathWithin(rp, rr) {
			return rr, true
		}
	}
	return "", false
}

// CheckBackupDirOutsideChild refuses a state or pre-image directory the
// confined child could reach (goodqa#15865 C5). dir is the directory the
// runner will create under; it need not exist yet.
func CheckBackupDirOutsideChild(dir string, roots []string) error {
	if root, under := UnderAnyRoot(dir, roots); under {
		return fmt.Errorf("native: state directory %q resolves under %q, which the confined child can write; "+
			"the pre-image copies would not survive a hostile child. Set FABRIC_STATE_DIR to a directory outside "+
			"the worktree, the run directory and the temp roots (the default is ~/.local/state/fabric)", dir, root)
	}
	return nil
}
