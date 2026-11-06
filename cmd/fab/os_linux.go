//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"fabric/internal/host"
)

func newLinuxProfile(self string) host.Profile {
	return &host.Linux{Self: self, ExtraRW: []string{filepath.Join(os.Getenv("HOME"), ".cache"), filepath.Join(os.Getenv("HOME"), ".claude")}}
}

func newDarwinProfile(string) host.Profile { return nil }

// sandboxMain: `fab __sandbox --rw DIR ... -- argv`. Applies Landlock to this
// process, then execs argv in place. Fails closed: no Landlock, no exec.
func sandboxMain(args []string) error {
	var rw []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--rw":
			if i+1 >= len(args) {
				return errors.New("--rw needs a path")
			}
			rw = append(rw, args[i+1])
			i += 2
		case "--":
			i++
			if i >= len(args) {
				return errors.New("no command after --")
			}
			if err := host.ApplyLandlock(rw); err != nil {
				return err
			}
			path, err := exec.LookPath(args[i])
			if err != nil {
				return err
			}
			return syscall.Exec(path, args[i:], os.Environ())
		default:
			return errors.New("usage: __sandbox --rw DIR ... -- argv")
		}
	}
	return errors.New("usage: __sandbox --rw DIR ... -- argv")
}
