package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/tools"
)

// C5 (goodqa#15865): a backup directory the confined child can reach is
// refused, whichever way it is named.
func TestBackupDirRefusedWhenChildCanWrite(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(base, "wt")
	run := filepath.Join(base, "run")
	extra := filepath.Join(base, "extra")
	tmpdir := filepath.Join(base, "mytmp")
	for _, d := range []string{wt, run, extra, tmpdir, filepath.Join(wt, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmpdir)
	link := filepath.Join(base, "link-into-wt")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	roots := tools.ChildWritableRoots(wt, run, []string{extra})
	for _, c := range []struct{ name, dir string }{
		{"worktree", filepath.Join(wt, "state")},
		{"worktree itself", wt},
		{"nested under worktree, not yet created", filepath.Join(wt, "a", "b", "state")},
		{"run dir", filepath.Join(run, "state")},
		{"extra writable root", filepath.Join(extra, "state")},
		{"TMPDIR", filepath.Join(tmpdir, "state")},
		{"/tmp", "/tmp/fabric-state"},
		{"/private/tmp", "/private/tmp/fabric-state"},
		{"/var/tmp", "/var/tmp/fabric-state"},
		{"/private/var/folders", "/private/var/folders/zz/fabric-state"},
		{"os.TempDir()", filepath.Join(os.TempDir(), "fabric-state")},
		{"symlink into the worktree", filepath.Join(link, "state")},
		{"dot-dot back into the worktree", filepath.Join(base, "x", "..", "wt", "state")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := tools.CheckBackupDirOutsideChild(c.dir, roots); err == nil {
				t.Fatalf("%s accepted", c.dir)
			} else if !strings.Contains(err.Error(), "child can write") {
				t.Fatalf("unclear error: %v", err)
			}
		})
	}
	if err := tools.CheckBackupDirOutsideChild("relative/state", roots); err == nil {
		t.Fatal("a relative directory is refused (unresolvable)")
	}
}

func TestBackupDirDefaultAccepted(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	def := filepath.Join(home, ".local", "state", "fabric")
	base, _ := filepath.EvalSymlinks(t.TempDir())
	roots := tools.ChildWritableRoots(filepath.Join(base, "wt"), filepath.Join(base, "run"), nil)
	if _, under := tools.UnderAnyRoot(def, roots); under {
		t.Skipf("home %s is itself under a temp root on this host", home)
	}
	if err := tools.CheckBackupDirOutsideChild(def, roots); err != nil {
		t.Fatalf("the default state dir must be accepted: %v", err)
	}
}
