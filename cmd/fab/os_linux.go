//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fabric/internal/egress"
	"fabric/internal/host"
)

// newLinuxProfile: read-only toolchain roots, no writable roots in $HOME
// (review C2), TCP connect limited to 443 and the database port the fab
// binary itself needs (review H3). The user's Claude configuration and
// caches are never granted; the launch line gives each run private ones.
//
// FABRIC_LINUX_WORKER_MANAGER=1 selects the worker manager instead
// (goodqa#15235 S5b): a per-run provider-only loopback mediator and no
// direct TCP allowance. Any other value keeps the legacy profile, and the
// manager is then never constructed.
func newLinuxProfile(self string) host.Profile {
	home := os.Getenv("HOME")
	ro := []string{"/usr/local/go", filepath.Join(home, "go", "pkg", "mod"), filepath.Join(home, ".local", "bin"), filepath.Join(home, ".local", "share", "fabric")}
	if os.Getenv("FABRIC_LINUX_WORKER_MANAGER") == "1" {
		return newLinuxManagerProfile(self, ro)
	}
	return &host.Linux{Self: self, ReadOnly: ro, TCPPorts: workerTCPPorts(), User: os.Getenv("FABRIC_WORKER_USER")}
}

// newLinuxManagerProfile is a variable so a test can prove the flag-off
// path never reaches it.
var newLinuxManagerProfile = func(self string, ro []string) host.Profile {
	return &host.LinuxManager{Self: self, ReadOnly: ro, EgressConfig: &egress.Config{}, User: os.Getenv("FABRIC_WORKER_USER")}
}

func newDarwinProfile(string) host.Profile { return nil }

// managerSocketPreflight runs with the worker socket filter installed. Every
// fixed, abstract and nonstandard Unix address must fail with EACCES/EPERM:
// absence of a socket is not accepted as proof of manager isolation. UDP socket
// creation must fail the same way, closing the local-resolver DNS side channel.
func managerSocketPreflight(paths []string) error {
	if len(paths) == 0 {
		return errors.New("manager socket preflight: no sockets named")
	}
	addresses := append([]string(nil), paths...)
	addresses = append(addresses,
		fmt.Sprintf("@fab-manager-preflight-%d", os.Getpid()),
		filepath.Join(os.TempDir(), fmt.Sprintf("fab-nonstandard-bus-%d", os.Getpid())),
	)
	for _, path := range addresses {
		conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("manager socket preflight: %s is reachable", path)
		}
		if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("manager socket preflight: %s was not denied by policy: %w", path, err)
		}
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err == nil {
		_ = syscall.Close(fd)
		return errors.New("manager socket preflight: worker UDP is reachable")
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("manager socket preflight: UDP was not denied by policy: %w", err)
	}
	return nil
}

// procIsolationPreflight proves that the unit's PID namespace and procfs
// settings are effective, not merely accepted and ignored by systemd. The
// outside PID is supplied by the trusted controller; self metadata must remain
// usable for readiness and ordinary runtimes.
func procIsolationPreflight(outsidePID int) error {
	if outsidePID <= 0 {
		return errors.New("proc preflight: invalid outside PID")
	}
	if _, err := os.ReadFile("/proc/self/status"); err != nil {
		return fmt.Errorf("proc preflight: self metadata unavailable: %w", err)
	}
	if os.Getpid() != 1 {
		return fmt.Errorf("proc preflight: PrivatePIDs is ineffective (launcher PID=%d, want 1)", os.Getpid())
	}
	// ProcSubset=pid hides procfs entries that are not directly associated
	// with a process. Checking a stable non-PID entry catches kernels that
	// silently ignore the subset mount option.
	if _, err := os.Stat("/proc/uptime"); err == nil {
		return errors.New("proc preflight: ProcSubset=pid is ineffective (/proc/uptime is visible)")
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("proc preflight: ProcSubset check returned an inconclusive error: %w", err)
	}
	for _, name := range []string{"mem", "environ", "maps"} {
		path := fmt.Sprintf("/proc/%d/%s", outsidePID, name)
		file, err := os.Open(path)
		if err == nil {
			_ = file.Close()
			return fmt.Errorf("proc preflight: outside-unit %s is visible", path)
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("proc preflight: outside-unit %s returned an inconclusive error: %w", path, err)
		}
	}
	return nil
}

func managerIsolationPreflight(paths []string, outsidePID int) error {
	if err := managerSocketPreflight(paths); err != nil {
		return err
	}
	return procIsolationPreflight(outsidePID)
}

// egressSocketPreflight additionally proves the loopback proxy is reachable
// and the cgroup IP filter denies a non-loopback TCP connection. The TEST-NET-1
// address cannot be a legitimate provider; any result other than EACCES/EPERM
// is not accepted as evidence that the filter works.
func egressSocketPreflight(proxyValue string, outsidePID int, paths []string) error {
	if err := managerIsolationPreflight(paths, outsidePID); err != nil {
		return err
	}
	proxy, err := url.Parse(proxyValue)
	if err != nil || proxy.Scheme != "http" || proxy.Hostname() != "127.0.0.1" || proxy.Port() == "" {
		return errors.New("egress preflight: invalid loopback proxy URL")
	}
	conn, err := net.DialTimeout("tcp", proxy.Host, time.Second)
	if err != nil {
		return fmt.Errorf("egress preflight: loopback proxy unreachable: %w", err)
	}
	_ = conn.Close()
	direct, err := net.DialTimeout("tcp", "192.0.2.1:443", time.Second)
	if err == nil {
		_ = direct.Close()
		return errors.New("egress preflight: non-loopback TCP is reachable")
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("egress preflight: non-loopback TCP was not denied by policy: %w", err)
	}
	return nil
}

// Darwin only; set by `fab daemon` on every OS.
var (
	darwinLaunchRO []string
	darwinProvider string
)

// sandboxMain: `fab __sandbox --env-keep NAMES [--ro DIR]... [--rw DIR]... [--tcp PORT]... -- argv`.
// Applies Landlock to this process, then execs argv in place with exactly
// the NAMES of its environment (goodqa#15087 AC-6). Fails closed.
// Preflight modes only probe sockets and procfs; they never exec a command and
// cannot be used to bypass the worker sandbox.
func sandboxMain(args []string) error {
	if len(args) > 0 && (args[0] == "--manager-preflight" || args[0] == "--egress-preflight") {
		if err := host.ApplyWorkerSocketPolicy(); err != nil {
			return err
		}
		if args[0] == "--manager-preflight" {
			if len(args) < 4 || args[2] != "--" {
				return errors.New("usage: __sandbox --manager-preflight OUTSIDE_PID -- MANAGER_SOCKET...")
			}
			outsidePID, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("manager preflight outside PID: %w", err)
			}
			return managerIsolationPreflight(args[3:], outsidePID)
		}
		if len(args) < 5 || args[3] != "--" {
			return errors.New("usage: __sandbox --egress-preflight PROXY_URL OUTSIDE_PID -- MANAGER_SOCKET...")
		}
		outsidePID, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("egress preflight outside PID: %w", err)
		}
		return egressSocketPreflight(args[1], outsidePID, args[4:])
	}
	sb, argv, env, err := sandboxPlan(args)
	if err != nil {
		return err
	}
	if err := host.ApplyLandlock(sb); err != nil {
		return err
	}
	if err := host.CompleteSandboxReady(sb); err != nil {
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
		case "--env-keep", "--env-file", "--ro", "--rw", "--tcp", "--ready-file", "--ready-cgroup":
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
			case "--ready-file":
				sb.ReadyFile = args[i+1]
			case "--ready-cgroup":
				sb.ReadyCGroup = args[i+1]
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
