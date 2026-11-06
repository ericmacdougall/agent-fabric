//go:build linux

package host

import (
	"fmt"
	"os"
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
}

// DefaultReadOnly is the system part of the read allow-list; callers add the
// toolchains and module caches a worker needs.
var DefaultReadOnly = []string{"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/etc", "/opt", "/proc", "/sys", "/dev", "/tmp", "/var/lib", "/var/tmp", "/snap", "/run/systemd/resolve"}

// ApplyLandlock restricts the calling process (and everything it execs) to
// the sandbox. It fails closed: if Landlock is unavailable or a rule cannot
// be added, the caller must not launch.
func ApplyLandlock(sb Sandbox) error {
	abi, err := LandlockABI()
	if err != nil || abi < 1 {
		return fmt.Errorf("landlock unavailable (abi=%d): %v", abi, err)
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
	return nil
}
