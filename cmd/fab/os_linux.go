//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	return &host.Linux{Self: self, ReadOnly: ro, TCPPorts: workerTCPPorts(), User: os.Getenv("FABRIC_WORKER_USER")}
}

func newDarwinProfile(string) host.Profile { return nil }

// Darwin only; set by `fab daemon` on every OS.
var (
	darwinLaunchRO []string
	darwinProvider string
)

// sandboxMain: `fab __sandbox --env-keep NAMES [--ro DIR]... [--rw DIR]... [--tcp PORT]... -- argv`.
// Applies Landlock to this process, then execs argv in place with exactly
// the NAMES of its environment (goodqa#15087 AC-6). Fails closed.
func sandboxMain(args []string) error {
	sb, argv, env, err := sandboxPlan(args)
	if err != nil {
		return err
	}
	if err := host.ApplyLandlock(sb); err != nil {
		return err
	}
	// Resolve the command with the worker's PATH (from the environment
	// file); this process's own PATH was blanked by the launcher.
	os.Setenv("PATH", "")
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			os.Setenv("PATH", v)
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

const sandboxUsage = "usage: __sandbox --env-keep NAMES --env-file FILE [--ro DIR] [--rw DIR] [--tcp PORT] ... -- argv"

// sandboxPlan parses the __sandbox arguments and returns the Landlock rules,
// the worker argv and its exact environment: the pairs in --env-file, which
// must name exactly the --env-keep names, and nothing from the systemd
// manager. Without both flags it refuses (goodqa#15087 AC-6, goodqa#15177:
// values travel in the file, never on a command line).
func sandboxPlan(args []string) (host.Sandbox, []string, []string, error) {
	var sb host.Sandbox
	keep, haveKeep, file := "", false, ""
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--env-keep", "--env-file", "--ro", "--rw", "--tcp":
			if i+1 >= len(args) {
				return sb, nil, nil, errors.New(args[i] + " needs a value")
			}
			switch args[i] {
			case "--env-keep":
				keep, haveKeep = args[i+1], true
			case "--env-file":
				file = args[i+1]
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
			if !haveKeep || file == "" {
				return sb, nil, nil, errors.New("__sandbox: --env-keep and --env-file are required (the worker environment must be named and passed off the command line)")
			}
			env, err := host.ReadEnvFile(file, keep)
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
