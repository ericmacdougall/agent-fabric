//go:build linux

package host

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock access rights (include/uapi/linux/landlock.h). Spelled out here so
// the profile does not depend on which constants a given x/sys ships.
const (
	llExecute    = 1 << 0
	llWriteFile  = 1 << 1
	llReadFile   = 1 << 2
	llReadDir    = 1 << 3
	llRemoveDir  = 1 << 4
	llRemoveFile = 1 << 5
	llMakeChar   = 1 << 6
	llMakeDir    = 1 << 7
	llMakeReg    = 1 << 8
	llMakeSock   = 1 << 9
	llMakeFifo   = 1 << 10
	llMakeBlock  = 1 << 11
	llMakeSym    = 1 << 12
	llRefer      = 1 << 13 // ABI 2
	llTruncate   = 1 << 14 // ABI 3

	llNetBindTCP    = 1 << 0 // ABI 4
	llNetConnectTCP = 1 << 1 // ABI 4

	llCreateRulesetVersion = 1 << 0
	llRulePathBeneath      = 1
	llRuleNetPort          = 2
)

// Own copies of the uapi structs (ABI 4 layout), independent of x/sys.
type llRulesetAttr struct {
	HandledAccessFS  uint64
	HandledAccessNet uint64
}

type llPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

type llNetPortAttr struct {
	AllowedAccess uint64
	Port          uint64
}

func landlockCreateRuleset(attr *llRulesetAttr, size uintptr, flags uint32) (int, error) {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(attr)), size, uintptr(flags))
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

func landlockAddRule(fd int, ruleType uintptr, attr unsafe.Pointer) error {
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd), ruleType, uintptr(attr), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func landlockRestrictSelf(fd int) error {
	_, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// LandlockABI returns the kernel's Landlock ABI version, or an error when the
// LSM is unavailable.
func LandlockABI() (int, error) {
	return landlockCreateRuleset(nil, 0, llCreateRulesetVersion)
}

// Sandbox is what a worker may touch (review C2, H3): read-only roots instead
// of "/", writable leaves, and the TCP ports it may connect to. Everything
// else under the filesystem is denied, which covers $HOME secrets, the
// user's Claude configuration and tool caches.
type Sandbox struct {
	ReadOnly  []string
	ReadWrite []string
	TCPPorts  []int
	// ReadyFile and ReadyCGroup are passed only by the Linux worker manager
	// (goodqa#15235 S5b). Their presence selects manager containment: Landlock
	// ABI 4 TCP rules and the worker socket filter become mandatory. The
	// legacy Linux profile never sets them, so its behaviour is unchanged.
	ReadyFile   string
	ReadyCGroup string
}

// managerContainment reports whether sb is a worker-manager launch and so
// requires ABI 4 TCP containment and the worker socket filter.
func managerContainment(sb Sandbox) bool {
	return sb.ReadyFile != "" || sb.ReadyCGroup != ""
}

// sandboxMinABI is the lowest Landlock ABI a launch accepts: 1 for the legacy
// profile, 4 (TCP connect rules) for the worker manager.
func sandboxMinABI(sb Sandbox) int {
	if managerContainment(sb) {
		return 4
	}
	return 1
}

// CompleteSandboxReady proves the trusted confined launcher reached the exact
// unit cgroup before exec. The marker is created once, after all restrictions;
// a fast command can then exit without making Start mistake it for a unit that
// never became ready.
func CompleteSandboxReady(sb Sandbox) error {
	if sb.ReadyFile == "" && sb.ReadyCGroup == "" {
		return nil
	}
	if sb.ReadyFile == "" || sb.ReadyCGroup == "" {
		return fmt.Errorf("sandbox readiness marker configuration is incomplete")
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("sandbox readiness cgroup: %w", err)
	}
	if strings.TrimSpace(string(cgroup)) != "0::"+sb.ReadyCGroup {
		return fmt.Errorf("sandbox readiness cgroup %q does not match %q", strings.TrimSpace(string(cgroup)), sb.ReadyCGroup)
	}
	// The marker is not secret and must be readable by the Fabric controller
	// through the jobs directory's inherited group when the worker has another UID.
	fd, err := unix.Open(sb.ReadyFile, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o640)
	if err != nil {
		return fmt.Errorf("sandbox readiness marker: %w", err)
	}
	file := os.NewFile(uintptr(fd), sb.ReadyFile)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("sandbox readiness marker returned no file")
	}
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		return fmt.Errorf("sandbox readiness marker permissions: %w", err)
	}
	// PrivatePIDs can give this process a namespace-local PID that has no
	// meaning in the controller's procfs. The trusted cgroup check above and
	// exclusive marker are the readiness proof; no host PID is serialized.
	if _, err := file.WriteString("ready\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("sandbox readiness marker write: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("sandbox readiness marker close: %w", err)
	}
	return nil
}

// DefaultReadOnly is the system part of the read allow-list; callers add the
// toolchains and module caches a worker needs.
var DefaultReadOnly = []string{"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/etc", "/opt", "/proc", "/sys", "/dev", "/tmp", "/var/lib", "/var/tmp", "/snap", "/run/systemd/resolve"}

// ApplyLandlock restricts the calling process (and everything it execs) to
// the sandbox. It fails closed: if Landlock is unavailable or a rule cannot
// be added, the caller must not launch.
func ApplyLandlock(sb Sandbox) error {
	abi, err := LandlockABI()
	if minABI := sandboxMinABI(sb); err != nil || abi < minABI {
		return fmt.Errorf("landlock unavailable (abi=%d, need %d): %v", abi, minABI, err)
	}
	read := uint64(llExecute | llReadFile | llReadDir)
	write := uint64(llWriteFile | llRemoveDir | llRemoveFile | llMakeChar | llMakeDir | llMakeReg | llMakeSock | llMakeFifo | llMakeBlock | llMakeSym)
	if abi >= 2 {
		write |= llRefer
	}
	if abi >= 3 {
		write |= llTruncate
	}
	attr := llRulesetAttr{HandledAccessFS: read | write}
	if abi >= 4 {
		attr.HandledAccessNet = llNetBindTCP | llNetConnectTCP
	}
	fd, err := landlockCreateRuleset(&attr, unsafe.Sizeof(attr), 0)
	if err != nil {
		return fmt.Errorf("landlock ruleset: %w", err)
	}
	defer unix.Close(fd)
	addPath := func(path string, access uint64) error {
		pfd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer unix.Close(pfd)
		beneath := llPathBeneathAttr{AllowedAccess: access, ParentFD: int32(pfd)}
		return landlockAddRule(fd, llRulePathBeneath, unsafe.Pointer(&beneath))
	}
	for _, p := range append(append([]string{}, DefaultReadOnly...), sb.ReadOnly...) {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue // a missing read root is simply not granted
		}
		if err := addPath(p, read); err != nil {
			return err
		}
	}
	for _, p := range sb.ReadWrite {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue
		}
		if err := addPath(p, read|write); err != nil {
			return err
		}
	}
	// Device nodes workers need for stdio redirection. Only file-applicable
	// rights may be granted on a non-directory; a failed rule is an error.
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/tty"} {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		if err := addPath(dev, llReadFile|llWriteFile); err != nil {
			return fmt.Errorf("landlock rule for %s: %w", dev, err)
		}
	}
	if abi >= 4 {
		for _, port := range sb.TCPPorts {
			if port <= 0 || port > 65535 {
				return fmt.Errorf("landlock: tcp port %d out of range", port)
			}
			np := llNetPortAttr{AllowedAccess: llNetConnectTCP, Port: uint64(port)}
			if err := landlockAddRule(fd, llRuleNetPort, unsafe.Pointer(&np)); err != nil {
				return fmt.Errorf("landlock net rule for port %d: %w", port, err)
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := landlockRestrictSelf(fd); err != nil {
		return fmt.Errorf("landlock restrict: %w", err)
	}
	if managerContainment(sb) {
		if err := ApplyWorkerSocketPolicy(); err != nil {
			return fmt.Errorf("worker protocol containment: %w", err)
		}
	}
	return nil
}
