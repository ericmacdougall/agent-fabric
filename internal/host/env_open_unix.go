//go:build !windows

package host

import (
	"errors"
	"os"
	"syscall"
)

// openEnvFile opens a worker environment file without following a final
// symlink, so a planted link cannot redirect the secret write or read
// (goodqa#15235 S5; the Kimi 6ac28d9 patch called this but never defined it).
func openEnvFile(path string, flag int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
}

// envFileLinks is the open file's remaining hard-link count.
func envFileLinks(f *os.File) (uint64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no link count for the environment file")
	}
	return uint64(s.Nlink), nil
}
