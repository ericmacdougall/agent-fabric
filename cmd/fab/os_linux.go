//go:build linux

package main

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"fabric/internal/host"
)

// newLinuxProfile: read-only toolchain roots, no writable roots in $HOME
// (review C2), TCP connect limited to 443 and the database port the fab
// binary itself needs (review H3). The user's Claude configuration and
// caches are never granted; the launch line gives each run private ones.
func newLinuxProfile(self string) host.Profile {
	home := os.Getenv("HOME")
	ro := []string{"/usr/local/go", filepath.Join(home, "go", "pkg", "mod"), filepath.Join(home, ".local", "bin"), filepath.Join(home, ".local", "share", "fabric")}
	ports := []int{443}
	if u, err := url.Parse(os.Getenv("FABRIC_DSN")); err == nil && u.Port() != "" {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			ports = append(ports, p)
		}
	}
	return &host.Linux{Self: self, ReadOnly: ro, TCPPorts: ports, User: os.Getenv("FABRIC_WORKER_USER")}
}

func newDarwinProfile(string) host.Profile { return nil }

// sandboxMain: `fab __sandbox [--ro DIR]... [--rw DIR]... [--tcp PORT]... -- argv`.
// Applies Landlock to this process, then execs argv in place. Fails closed.
func sandboxMain(args []string) error {
	var sb host.Sandbox
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--ro", "--rw", "--tcp":
			if i+1 >= len(args) {
				return errors.New(args[i] + " needs a value")
			}
			switch args[i] {
			case "--ro":
				sb.ReadOnly = append(sb.ReadOnly, args[i+1])
			case "--rw":
				sb.ReadWrite = append(sb.ReadWrite, args[i+1])
			case "--tcp":
				p, err := strconv.Atoi(args[i+1])
				if err != nil {
					return err
				}
				sb.TCPPorts = append(sb.TCPPorts, p)
			}
			i += 2
		case "--":
			i++
			if i >= len(args) {
				return errors.New("no command after --")
			}
			if err := host.ApplyLandlock(sb); err != nil {
				return err
			}
			path, err := exec.LookPath(args[i])
			if err != nil {
				return err
			}
			return syscall.Exec(path, args[i:], os.Environ())
		default:
			return errors.New("usage: __sandbox [--ro DIR] [--rw DIR] [--tcp PORT] ... -- argv")
		}
	}
	return errors.New("usage: __sandbox [--ro DIR] [--rw DIR] [--tcp PORT] ... -- argv")
}
