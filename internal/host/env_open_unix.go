//go:build !windows

package host

import (
	"os"
	"syscall"
)

// openEnvFile opens a worker environment file without following a final
// symlink, so a planted link cannot redirect the secret write or read
// (goodqa#15235 S5; the Kimi 6ac28d9 patch called this but never defined it).
func openEnvFile(path string, flag int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
}
