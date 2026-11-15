//go:build linux

package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const goneUID = 997

// goneProc is one fake /proc entry.
type goneProc struct {
	pid, uid int
	cgroup   string // the 0:: path
	startSec int64  // seconds after boot
	exe      string
}

type goneFixture struct {
	procs        []goneProc
	noManager    bool // no user@uid.service cgroup directory
	unitPresent  bool // the run's unit cgroup still exists
	linger       string
	state        string
	lingerErr    error
	exeUID       int
	exeMode      os.FileMode
	attemptAfter int64 // attempt start, seconds after boot
	hz           int64 // the kernel's USER_HZ; 0 means 100
}

const goneBoot = 1_700_000_000

func mgr(rel string) string {
	return fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/%s", goneUID, goneUID, rel)
}

func (f goneFixture) run(t *testing.T) (StopReceipt, error) {
	t.Helper()
	root := t.TempDir()
	proc, cg := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	manager := filepath.Join(cg, "user.slice", fmt.Sprintf("user-%d.slice", goneUID), fmt.Sprintf("user@%d.service", goneUID))
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(proc, 0o755))
	if !f.noManager {
		must(os.MkdirAll(filepath.Join(manager, "app.slice"), 0o755))
	}
	if f.unitPresent {
		must(os.MkdirAll(filepath.Join(manager, "app.slice", mgrUnitName("r_gone_1")), 0o755))
	}
	must(os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu  1 2 3\nbtime "+strconv.Itoa(goneBoot)+"\nprocesses 9\n"), 0o644))
	exes := map[int]string{}
	for _, p := range f.procs {
		d := filepath.Join(proc, strconv.Itoa(p.pid))
		must(os.MkdirAll(d, 0o755))
		must(os.WriteFile(filepath.Join(d, "status"), []byte(fmt.Sprintf("Name:\tx\nUid:\t%d\t%d\t%d\t%d\n", p.uid, p.uid, p.uid, p.uid)), 0o644))
		must(os.WriteFile(filepath.Join(d, "cgroup"), []byte("0::"+p.cgroup+"\n"), 0o644))
		stat := fmt.Sprintf("%d (x) S 1 %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0", p.pid, p.pid, p.pid, p.startSec*clockTicks)
		must(os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644))
		must(os.WriteFile(filepath.Join(d, "cmdline"), []byte("x\x00"), 0o644))
		exes[p.pid] = p.exe
	}
	linger, state := f.linger, f.state
	if linger == "" { // with lingerErr too, so only the error refuses it
		linger, state = "yes", "lingering"
	}
	mode := f.exeMode
	if mode == 0 {
		mode = 0o755
	}
	p := goneProbe{procRoot: proc, cgroupRoot: cg,
		readExe:  func(pid int) (string, error) { return exes[pid], nil },
		exeOwner: func(string) (int, os.FileMode, error) { return f.exeUID, mode, nil },
		linger:   func() (string, string, error) { return linger, state, f.lingerErr },
		clkTck: func() (int64, error) {
			if f.hz != 0 {
				return f.hz, nil
			}
			return clockTicks, nil
		}}
	after := f.attemptAfter
	if after == 0 {
		after = 3600
	}
	var rc StopReceipt
	err := verifyGoneAt(p, goneUID, "r_gone_1", time.Unix(goneBoot+after, 0), &rc)
	rc.Verified = err == nil
	return rc, err
}

var (
	managerProcs = []goneProc{{pid: 1735, uid: goneUID, cgroup: mgr("init.scope"), startSec: 5}, {pid: 1739, uid: goneUID, cgroup: mgr("init.scope"), startSec: 5}}
	systemBus    = goneProc{pid: 2771, uid: goneUID, cgroup: mgr("session.slice/dbus.service"), startSec: 60, exe: "/usr/bin/dbus-daemon"}
	otherUser    = goneProc{pid: 400, uid: 1000, cgroup: "/user.slice/user-1000.slice/session-3.scope", startSec: 9}
)

func with(extra ...goneProc) []goneProc {
	return append(append(append([]goneProc{}, managerProcs...), otherUser), extra...)
}

// goodqa#15966 AC-35(i), E9: VerifyGone passes only for the manager alone or
// with one pre-existing root-owned system bus; every other state is
// unverified.
func TestVerifyGoneTable(t *testing.T) {
	late := systemBus
	late.startSec = 3600 + 10
	foreign := systemBus
	foreign.exe = "/home/fab-worker/.local/bin/dbus-daemon"
	second := systemBus
	second.pid = 2772
	cases := map[string]struct {
		f    goneFixture
		pass bool
	}{
		"manager alone":                {goneFixture{procs: with()}, true},
		"pre-start system dbus":        {goneFixture{procs: with(systemBus)}, true},
		"dbus started after the lease": {goneFixture{procs: with(late)}, false},
		"foreign exe in dbus.service":  {goneFixture{procs: with(foreign)}, false},
		"bus exe not root-owned":       {goneFixture{procs: with(systemBus), exeUID: goneUID}, false},
		"bus exe group-writable":       {goneFixture{procs: with(systemBus), exeMode: 0o775}, false},
		"second process in dbus":       {goneFixture{procs: with(systemBus, second)}, false},
		"stray in another unit":        {goneFixture{procs: with(goneProc{pid: 900, uid: goneUID, cgroup: mgr("app.slice/fab-other.service"), startSec: 70})}, false},
		"stray outside the manager":    {goneFixture{procs: with(goneProc{pid: 901, uid: goneUID, cgroup: "/user.slice/user-997.slice/session-4.scope", startSec: 70})}, false},
		"stray in the run's own unit":  {goneFixture{procs: with(goneProc{pid: 902, uid: goneUID, cgroup: mgr("app.slice/" + mgrUnitName("r_gone_1")), startSec: 70})}, false},
		"unit cgroup present":          {goneFixture{procs: with(), unitPresent: true}, false},
		"unreadable manager tree":      {goneFixture{procs: with(), noManager: true}, false},
		"linger off":                   {goneFixture{procs: with(), linger: "no", state: "closing"}, false},
		"a session is open":            {goneFixture{procs: with(), linger: "yes", state: "active"}, false},
		"linger unreadable":            {goneFixture{procs: with(), lingerErr: errors.New("bus timeout")}, false},
		"manager not visible":          {goneFixture{procs: []goneProc{otherUser}}, false},
		"USER_HZ is not 100":           {goneFixture{procs: with(systemBus), hz: 250}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rc, err := tc.f.run(t)
			if tc.pass && (err != nil || !rc.Verified) {
				t.Fatalf("want verified, got %v", err)
			}
			if !tc.pass && (err == nil || rc.Verified) {
				t.Fatalf("want unverified, got verified (escaped %d)", len(rc.Escaped))
			}
		})
	}
}

// Another run holding this manager's gate makes VerifyGone unverified
// before anything is read.
func TestVerifyGoneRefusesWhileAnotherRunHoldsTheGate(t *testing.T) {
	l := &LinuxManager{Self: "/srv/fabric/bin/fab", User: "fab-worker"}
	if !l.runGate.tryLock() {
		t.Fatal("fixture: gate busy")
	}
	defer l.runGate.unlock()
	rc, err := l.VerifyGone("r_gone_1", time.Now())
	if !errors.Is(err, ErrWorkerManagerBusy) || rc.Verified {
		t.Fatalf("err %v verified %v; want ErrWorkerManagerBusy, unverified", err, rc.Verified)
	}
}

// The live auxv of this test process carries AT_CLKTCK, and it is 100.
func TestAuxvClockTicksIsUserHZ(t *testing.T) {
	hz, err := auxvClockTicks("/proc/self/auxv")
	if err != nil || hz != clockTicks {
		t.Fatalf("AT_CLKTCK %d %v; want %d", hz, err, clockTicks)
	}
}
