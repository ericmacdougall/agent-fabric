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

// sandboxMain: `fab __sandbox --env-keep NAMES [--ro DIR]... [--rw DIR]... [--tcp PORT]... -- argv`.
// Applies Landlock to this process, then execs argv in place with exactly
// the NAMES of its environment (goodqa#15087 AC-6). Fails closed.
func sandboxMain(args []string) error {
	sb, argv, env, err := sandboxPlan(args, os.Environ())
	if err != nil {
		return err
	}
	if err := host.ApplyLandlock(sb); err != nil {
		return err
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

const sandboxUsage = "usage: __sandbox --env-keep NAMES [--ro DIR] [--rw DIR] [--tcp PORT] ... -- argv"

// sandboxPlan parses the __sandbox arguments and returns the Landlock rules,
// the worker argv and its exact environment: environ filtered to the
// --env-keep names. Without --env-keep it refuses, so a launcher that does
// not name the environment never passes the systemd manager's through.
func sandboxPlan(args, environ []string) (host.Sandbox, []string, []string, error) {
	var sb host.Sandbox
	keep, haveKeep := "", false
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--env-keep", "--ro", "--rw", "--tcp":
			if i+1 >= len(args) {
				return sb, nil, nil, errors.New(args[i] + " needs a value")
			}
			switch args[i] {
			case "--env-keep":
				keep, haveKeep = args[i+1], true
			case "--ro":
				sb.ReadOnly = append(sb.ReadOnly, args[i+1])
			case "--rw":
				sb.ReadWrite = append(sb.ReadWrite, args[i+1])
			case "--tcp":
				p, err := strconv.Atoi(args[i+1])
				if err != nil {
					return sb, nil, nil, err
				}
				sb.TCPPorts = append(sb.TCPPorts, p)
			}
			i += 2
		case "--":
			i++
			if i >= len(args) {
				return sb, nil, nil, errors.New("no command after --")
			}
			if !haveKeep {
				return sb, nil, nil, errors.New("__sandbox: --env-keep is required (the worker environment must be named)")
			}
			env, err := host.KeepEnv(environ, keep)
			if err != nil {
				return sb, nil, nil, err
			}
			return sb, args[i:], env, nil
		default:
			return sb, nil, nil, errors.New(sandboxUsage)
		}
	}
	return sb, nil, nil, errors.New(sandboxUsage)
}
