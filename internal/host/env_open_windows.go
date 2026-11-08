//go:build windows

package host

import (
	"errors"
	"os"
)

// openEnvFile: the environment-file protocol is not supported on Windows,
// so it fails closed (goodqa#15235 S5).
func openEnvFile(string, int, os.FileMode) (*os.File, error) {
	return nil, errors.New("worker environment files are not supported on windows")
}

func envFileLinks(*os.File) (uint64, error) {
	return 0, errors.New("worker environment files are not supported on windows")
}
