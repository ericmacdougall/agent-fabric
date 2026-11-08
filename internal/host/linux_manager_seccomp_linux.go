//go:build linux

package host

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Classic-BPF constants from linux/filter.h and linux/seccomp.h. Keeping the
// small filter here avoids a libseccomp/cgo dependency in the worker launcher.
const (
	bpfLoadWordAbs = 0x20
	bpfJumpEqK     = 0x15
	bpfJumpSetK    = 0x45
	bpfAluAndK     = 0x54
	bpfReturnK     = 0x06

	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	auditArchX86_64  = 0xc000003e
	auditArchAarch64 = 0xc00000b7
	x32SyscallBit    = 0x40000000
)

// ApplyWorkerSocketPolicy permits only TCP-capable sockets. Landlock and the
// cgroup IP filter then narrow those TCP sockets to the mediator. Datagram,
// raw, Unix, netlink and every other socket family are denied, closing both
// local-resolver DNS exfiltration and abstract/nonstandard D-Bus paths.
// io_uring_setup is denied because ring socket operations bypass socket(2), and
// x32 syscall numbers are rejected rather than allowed as unknown calls.
func ApplyWorkerSocketPolicy() error {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = auditArchX86_64
	case "arm64":
		arch = auditArchAarch64
	default:
		return fmt.Errorf("worker socket policy: unsupported architecture %s", runtime.GOARCH)
	}
	ret := func(value uint32) unix.SockFilter {
		return unix.SockFilter{Code: bpfReturnK, K: value}
	}
	filters := []unix.SockFilter{
		{Code: bpfLoadWordAbs, K: 4}, // 0: seccomp_data.arch
		{Code: bpfJumpEqK, Jt: 1, K: arch},
		ret(seccompRetKillProcess),
		{Code: bpfLoadWordAbs, K: 0}, // 3: seccomp_data.nr
		{Code: bpfJumpSetK, Jf: 1, K: x32SyscallBit},
		ret(seccompRetKillProcess),
		{Code: bpfJumpEqK, Jt: 4, K: uint32(unix.SYS_SOCKET)}, // to domain at 11
		{Code: bpfJumpEqK, Jf: 1, K: uint32(unix.SYS_SOCKETPAIR)},
		ret(seccompRetErrno | uint32(syscall.EPERM)),
		{Code: bpfJumpEqK, Jf: 13, K: uint32(unix.SYS_IO_URING_SETUP)}, // other syscalls to 23
		ret(seccompRetErrno | uint32(syscall.EPERM)),
		{Code: bpfLoadWordAbs, K: 16}, // 11: socket domain
		{Code: bpfJumpEqK, Jt: 2, K: unix.AF_INET},
		{Code: bpfJumpEqK, Jt: 1, K: unix.AF_INET6},
		ret(seccompRetErrno | uint32(syscall.EPERM)),
		{Code: bpfLoadWordAbs, K: 24}, // 15: socket type
		{Code: bpfAluAndK, K: 0xf},    // strip SOCK_CLOEXEC/SOCK_NONBLOCK
		{Code: bpfJumpEqK, Jt: 1, K: unix.SOCK_STREAM},
		ret(seccompRetErrno | uint32(syscall.EPERM)),
		{Code: bpfLoadWordAbs, K: 32}, // 19: socket protocol
		{Code: bpfJumpEqK, Jt: 2, K: 0},
		{Code: bpfJumpEqK, Jt: 1, K: unix.IPPROTO_TCP},
		ret(seccompRetErrno | uint32(syscall.EPERM)),
		ret(seccompRetAllow), // 23
	}
	if err := installWorkerSeccomp(filters); err != nil {
		return fmt.Errorf("worker socket policy: %w", err)
	}

	// A separate default-allow filter denies cross-process inspection, memory
	// access, descriptor theft and signals. It also covers same-UID managers and
	// waiters, where filesystem and IP controls provide no process boundary.
	processFilters := []unix.SockFilter{
		{Code: bpfLoadWordAbs, K: 4},
		{Code: bpfJumpEqK, Jt: 1, K: arch},
		ret(seccompRetKillProcess),
		{Code: bpfLoadWordAbs, K: 0},
		{Code: bpfJumpSetK, Jf: 1, K: x32SyscallBit},
		ret(seccompRetKillProcess),
	}
	for _, number := range []uint32{
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_GETFD, unix.SYS_PIDFD_SEND_SIGNAL,
		unix.SYS_PROCESS_MADVISE, unix.SYS_PROCESS_MRELEASE,
		unix.SYS_KCMP, unix.SYS_PERF_EVENT_OPEN,
		unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_TGKILL,
		unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO,
		unix.SYS_SCHED_SETAFFINITY, unix.SYS_SETPRIORITY, unix.SYS_IOPRIO_SET,
	} {
		processFilters = append(processFilters,
			unix.SockFilter{Code: bpfJumpEqK, Jf: 1, K: number},
			ret(seccompRetErrno|uint32(syscall.EPERM)),
		)
	}
	processFilters = append(processFilters, ret(seccompRetAllow))
	if err := installWorkerSeccomp(processFilters); err != nil {
		return fmt.Errorf("worker cross-process policy: %w", err)
	}
	return nil
}

func installWorkerSeccomp(filters []unix.SockFilter) error {
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return nil
}
