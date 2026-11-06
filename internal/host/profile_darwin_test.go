//go:build darwin

package host

import (
	"os"
	"os/exec"
	"testing"
)

func testProfile(t *testing.T) Profile {
	t.Helper()
	return &Darwin{SandboxExec: lookSandbox(t)}
}

func lookSandbox(t *testing.T) string {
	t.Helper()
	sb, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Fatal("sandbox-exec not found")
	}
	return sb
}

func darwinProfile(t *testing.T) interface {
	Start(Grant, []string, []string, *os.File, *os.File) (Handle, error)
	Wait(Handle) (int, error)
} {
	return &Darwin{SandboxExec: lookSandbox(t)}
}
