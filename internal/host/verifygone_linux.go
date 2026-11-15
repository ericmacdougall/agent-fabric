//go:build linux

package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fabric/internal/domain"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat starttime; Linux
// fixes it at 100 for userspace on every architecture fabd runs on. The
// kernel's value (AT_CLKTCK in this process's auxv, what getconf CLK_TCK
// prints) is read without cgo and compared: a mismatch or an unreadable
// auxv is unverified, never a guessed start time.
const clockTicks = 100

// atClkTck is the auxv type of AT_CLKTCK.
const atClkTck = 17

// auxvClockTicks reads AT_CLKTCK from an auxv image of native-endian word
// pairs (amd64 and arm64: 8-byte little-endian words).
func auxvClockTicks(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for i := 0; i+16 <= len(data); i += 16 {
		typ, val := binary.LittleEndian.Uint64(data[i:]), binary.LittleEndian.Uint64(data[i+8:])
		if typ == 0 {
			break
		}
		if typ == atClkTck {
			return int64(val), nil
		}
	}
	return 0, errors.New("no AT_CLKTCK in auxv")
}

// systemBusExes are the system message-bus binaries a pre-existing session
// bus may run (owner ruling on E: exactly these, root-owned).
var systemBusExes = map[string]bool{"/usr/bin/dbus-daemon": true, "/usr/bin/dbus-broker": true, "/usr/bin/dbus-broker-launch": true}

// goneProbe is everything VerifyGone reads; tests replace each part.
type goneProbe struct {
	procRoot, cgroupRoot string
	readExe              func(pid int) (string, error) // /proc/<pid>/exe, read as the worker
	exeOwner             func(path string) (uid int, mode os.FileMode, err error)
	linger               func() (linger, state string, err error) // loginctl show-user
	clkTck               func() (int64, error)                    // the kernel's USER_HZ
}

// VerifyGone stands in for W1's in-memory verified Stop after a fabd
// restart (goodqa#15966 AC-35(i), erratum E9), when no authenticated handle
// or unit baseline survives. It reports Verified only when all hold:
//  1. the run's unit cgroup is absent under a readable worker-manager tree;
//  2. every worker-uid process on the host is the manager itself
//     (user@uid.service/init.scope), except at most one system bus in
//     session.slice/dbus.service: a root-owned system dbus binary, started
//     before attemptStart, alone in that unit;
//  3. the manager is visible (a /proc that hides other users' processes
//     cannot prove absence) and fab-worker has no session (Linger=yes,
//     State=lingering);
//  4. this manager's run gate and the cross-process worker-manager flock
//     are free, and are held across the check.
//
// Whether another attempt is live in the store is the caller's check.
// Anything unreadable is unverified; there is no narrower fallback.
func (l *LinuxManager) VerifyGone(run domain.AttemptID, attemptStart time.Time) (StopReceipt, error) {
	rc := StopReceipt{Run: run, Reason: "restart: verify the runner is gone", StartedAt: time.Now()}
	done := func(err error) (StopReceipt, error) {
		rc.FinishedAt = time.Now()
		rc.Verified = err == nil
		return rc, err
	}
	if l.User == "" {
		return done(errors.New("verify gone: needs a dedicated worker account"))
	}
	if !l.runGate.tryLock() {
		return done(fmt.Errorf("%w: verify gone: another run holds the worker manager", ErrWorkerManagerBusy))
	}
	uid, err := l.workerUID()
	if err == nil {
		err = l.lockWorkerManager(uid)
	}
	if err != nil {
		l.runGate.unlock()
		return done(fmt.Errorf("verify gone: %w", err))
	}
	defer l.releaseRun() //nolint:errcheck // a failed unlock leaves the profile refused
	return done(verifyGoneAt(l.goneProbe(), uid, run, attemptStart, &rc))
}

func (l *LinuxManager) goneProbe() goneProbe {
	return goneProbe{procRoot: "/proc", cgroupRoot: "/sys/fs/cgroup",
		readExe: func(pid int) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
			defer cancel()
			out, err := l.asWorkerContext(ctx, "readlink", fmt.Sprintf("/proc/%d/exe", pid)).Output()
			return strings.TrimSpace(string(out)), err
		},
		exeOwner: func(path string) (int, os.FileMode, error) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, 0, err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, 0, errors.New("no stat owner")
			}
			return int(st.Uid), fi.Mode(), nil
		},
		clkTck: func() (int64, error) { return auxvClockTicks("/proc/self/auxv") },
		linger: func() (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), l.managerTimeout())
			defer cancel()
			out, err := l.command(ctx, "loginctl", "show-user", l.User, "-p", "Linger", "-p", "State").Output()
			if err != nil {
				return "", "", err
			}
			var lg, st string
			for _, line := range strings.Split(string(out), "\n") {
				if v, ok := strings.CutPrefix(line, "Linger="); ok {
					lg = strings.TrimSpace(v)
				} else if v, ok := strings.CutPrefix(line, "State="); ok {
					st = strings.TrimSpace(v)
				}
			}
			return lg, st, nil
		}}
}

func verifyGoneAt(p goneProbe, uid int, run domain.AttemptID, attemptStart time.Time, rc *StopReceipt) error {
	manager := filepath.Join(p.cgroupRoot, "user.slice", fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid))
	if fi, err := os.Stat(manager); err != nil || !fi.IsDir() {
		return fmt.Errorf("verify gone: worker-manager cgroup tree unreadable: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(manager, "app.slice", mgrUnitName(string(run)))); err == nil {
		return errors.New("verify gone: the run's unit cgroup still exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify gone: unit cgroup unreadable: %w", err)
	}
	lg, state, err := p.linger()
	if err != nil {
		return fmt.Errorf("verify gone: linger state unreadable: %w", err)
	}
	if lg != "yes" || state != "lingering" {
		return fmt.Errorf("verify gone: worker account Linger=%q State=%q, want a lingering manager with no session", lg, state)
	}
	btime, err := bootTime(p.procRoot)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(p.procRoot)
	if err != nil {
		return fmt.Errorf("verify gone: read proc: %w", err)
	}
	prefix := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/", uid, uid)
	managerSeen := false
	var bus []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		owner, exists, err := procUIDAt(p.procRoot, pid)
		if err != nil {
			return fmt.Errorf("verify gone: %w", err)
		}
		if !exists || owner != uid {
			continue
		}
		cg, exists, err := readProcFile(fmt.Sprintf("%s/%d/cgroup", p.procRoot, pid))
		if err != nil {
			return fmt.Errorf("verify gone: %w", err)
		}
		proc, pexists, perr := procIdentityAt(p.procRoot, pid)
		if perr != nil {
			return fmt.Errorf("verify gone: %w", perr)
		}
		if !exists || !pexists {
			continue // exited during the scan
		}
		path, ok := strings.CutPrefix(strings.TrimSpace(string(cg)), "0::")
		switch rel := strings.TrimPrefix(path, prefix); {
		case !ok:
			return fmt.Errorf("verify gone: pid %d has a malformed cgroup record", pid)
		case !strings.HasPrefix(path, prefix):
			proc.Escaped = true
			rc.Escaped = append(rc.Escaped, proc) // outside the worker manager
		case rel == "init.scope":
			managerSeen = true
		case rel == "session.slice/dbus.service":
			bus = append(bus, proc)
		default:
			proc.Escaped = true
			rc.Escaped = append(rc.Escaped, proc)
		}
	}
	if len(rc.Escaped) > 0 {
		return fmt.Errorf("verify gone: %d worker-uid process(es) outside the manager", len(rc.Escaped))
	}
	if !managerSeen {
		return errors.New("verify gone: the worker manager is not visible in /proc; absence cannot be proven")
	}
	switch len(bus) {
	case 0:
		return nil
	case 1:
	default:
		rc.Survivors = bus
		return fmt.Errorf("verify gone: %d processes in the session bus unit, want at most 1", len(bus))
	}
	b := bus[0]
	exe, err := p.readExe(b.PID)
	if err != nil || !systemBusExes[exe] {
		rc.Survivors = bus
		return fmt.Errorf("verify gone: session bus pid %d is not a system bus binary", b.PID)
	}
	if owner, mode, err := p.exeOwner(exe); err != nil || owner != 0 || mode.Perm()&0o022 != 0 {
		rc.Survivors = bus
		return fmt.Errorf("verify gone: %s is not root-owned and protected", exe)
	}
	if hz, err := p.clkTck(); err != nil || hz != clockTicks {
		rc.Survivors = bus
		return fmt.Errorf("verify gone: USER_HZ %d (%v), want %d; the bus start time cannot be read", hz, err, clockTicks)
	}
	ticks, err := strconv.ParseInt(b.Start, 10, 64)
	if err != nil {
		return fmt.Errorf("verify gone: session bus start: %w", err)
	}
	if btime*clockTicks+ticks >= attemptStart.Unix()*clockTicks {
		rc.Survivors = bus
		return fmt.Errorf("verify gone: session bus pid %d started during the attempt", b.PID)
	}
	return nil
}

// bootTime is /proc/stat btime, in seconds since the epoch.
func bootTime(procRoot string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, fmt.Errorf("verify gone: boot time: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("verify gone: no btime in /proc/stat")
}
