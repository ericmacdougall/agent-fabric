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

	llCreateRulesetVersion = 1 << 0
	llRulePathBeneath      = 1
)

func landlockCreateRuleset(attr *unix.LandlockRulesetAttr, size uintptr, flags uint32) (int, error) {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(attr)), size, uintptr(flags))
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

func landlockAddPathRule(fd int, attr *unix.LandlockPathBeneathAttr) error {
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd), llRulePathBeneath, uintptr(unsafe.Pointer(attr)), 0, 0, 0)
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

// ApplyLandlock restricts the calling process (and everything it execs) to
// read-only access everywhere except the rw paths (ABI 4 on t2-dev, E5). It
// fails closed: if Landlock is unavailable the caller must not launch.
func ApplyLandlock(rw []string) error {
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
	attr := unix.LandlockRulesetAttr{Access_fs: read | write}
	fd, err := landlockCreateRuleset(&attr, unsafe.Sizeof(attr), 0)
	if err != nil {
		return fmt.Errorf("landlock ruleset: %w", err)
	}
	defer unix.Close(fd)
	addRule := func(path string, access uint64) error {
		pfd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer unix.Close(pfd)
		beneath := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pfd)}
		return landlockAddPathRule(fd, &beneath)
	}
	if err := addRule("/", read); err != nil {
		return err
	}
	for _, p := range rw {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue // a missing writable path is simply not granted
		}
		if err := addRule(p, read|write); err != nil {
			return err
		}
	}
	// Device nodes workers need for stdio redirection. Only file-applicable
	// rights may be granted on a non-directory (anything else is EINVAL), and
	// a failed rule is an error, never silence (R28).
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/tty"} {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		if err := addRule(dev, llReadFile|llWriteFile); err != nil {
			return fmt.Errorf("landlock rule for %s: %w", dev, err)
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
