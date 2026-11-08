//go:build !windows

package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodqa#15235 S5a: ReadEnvFile returns the worker's secrets only after the
// file is unlinked and verified gone. A file the reader cannot remove would
// stay readable for the whole run, so the exec is refused instead.
func TestReadEnvFileRefusesWhenUnlinkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can unlink in a read-only directory")
	}
	dir := filepath.Join(t.TempDir(), "jobs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".fab-env-r_s5a")
	if err := WriteEnvFile(p, []string{"HOME=/h", "PATH=/bin"}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // no unlink in this directory
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck
	env, err := ReadEnvFile(p, "HOME,PATH")
	if err == nil || !strings.Contains(err.Error(), "unlink before exec") {
		t.Fatalf("ReadEnvFile returned %d entries with err %v; want a refusal because the file could not be removed", len(env), err)
	}
	if env != nil {
		t.Fatal("environment values were returned although cleanup failed")
	}
	if _, statErr := os.Lstat(p); statErr != nil {
		t.Fatalf("test precondition: the file should still exist: %v", statErr)
	}
}

// The normal path still returns the exact environment and leaves no file.
func TestReadEnvFileRemovesFileBeforeReturning(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".fab-env-r_s5a_ok")
	if err := WriteEnvFile(p, []string{"HOME=/h", "PATH=/bin"}, false); err != nil {
		t.Fatal(err)
	}
	env, err := ReadEnvFile(p, "HOME,PATH")
	if err != nil || len(env) != 2 {
		t.Fatalf("env %q err %v", env, err)
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment file remains after ReadEnvFile: %v", err)
	}
}

// A symlink at the env-file path is refused for both write and read.
func TestEnvFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("HOME=/h"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".fab-env-r_link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(link, []string{"HOME=/h"}, false); err == nil {
		t.Fatal("WriteEnvFile wrote through a symlink")
	}
	if _, err := ReadEnvFile(link, "HOME"); err == nil {
		t.Fatal("ReadEnvFile read through a symlink")
	}
}
