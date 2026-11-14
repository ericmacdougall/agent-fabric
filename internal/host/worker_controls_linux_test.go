//go:build linux

package host

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeManagerHost models the legacy profile on a worker-account host for the
// manager controls: the probe scripts run nowhere, and the answer depends
// only on which uid's bus and which pid each script targets.
type fakeManagerHost struct {
	workerUID     int
	workerMgrPID  int
	workerBusOpen bool // review C1: the worker reaches its own manager's bus
	workerSignal  bool // and can signal its own manager
	uidErr        error
	pidErr        error

	scripts  []string
	pidAsked []int
	stops    []string
}

func (f *fakeManagerHost) hooks() *linuxControlHooks {
	return &linuxControlHooks{
		run: func(_ Grant, s string) (string, int, error) {
			f.scripts = append(f.scripts, s)
			workerBus := fmt.Sprintf("/run/user/%d/bus", f.workerUID)
			switch {
			case strings.Contains(s, "socket.AF_UNIX") && strings.Contains(s, workerBus):
				if f.workerBusOpen {
					return "bus=connected\nunit=created", 0, nil
				}
				return "bus=denied\nunit=refused", 0, nil
			case strings.Contains(s, "socket.AF_UNIX"):
				// any other uid's bus: a different uid's 0700 runtime dir
				return "bus=denied\nunit=refused", 0, nil
			case strings.Contains(s, "systemd-run"):
				return "unit=denied", 0, nil // the operator's manager (control 8)
			case strings.Contains(s, fmt.Sprintf(`" %d`, f.workerMgrPID)):
				if f.workerSignal {
					return "signal=allowed", 0, nil
				}
				return "signal=denied", 0, nil
			default:
				return "signal=denied", 0, nil // the operator's manager (control 9)
			}
		},
		workerUID: func() (int, error) { return f.workerUID, f.uidErr },
		managerPID: func(uid int) (int, error) {
			f.pidAsked = append(f.pidAsked, uid)
			if f.pidErr != nil {
				return 0, f.pidErr
			}
			if uid != f.workerUID {
				return 0, errors.New("no manager for that uid")
			}
			return f.workerMgrPID, nil
		},
		stopUnit: func(asWorker bool, unit string) {
			f.stops = append(f.stops, fmt.Sprintf("worker=%v:%s", asWorker, unit))
		},
	}
}

func resultsByName(rs []ControlResult) map[string]ControlResult {
	m := map[string]ControlResult{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

// goodqa#15976 AC-1 / NEG-1: with FABRIC_WORKER_USER set, controls 8 and 9
// must also probe the WORKER uid's own manager bus and manager process. On
// the legacy profile those are reachable (C1, confirmed live on t2-dev), so
// the worker controls must fail and Capabilities must refuse containment. A
// variant that probes os.Getuid() instead sees the operator's 0700 bus and
// an unsignallable foreign manager, passes, and fails this test.
func TestManagerControlsTargetTheWorkerUID(t *testing.T) {
	op := os.Getuid()
	f := &fakeManagerHost{workerUID: op + 4243, workerMgrPID: 777001, workerBusOpen: true, workerSignal: true}
	l := &Linux{User: "fab-worker", ctl: f.hooks()}
	rs, err := l.managerControls(controlGrant(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	by := resultsByName(rs)
	for _, n := range []string{"controlling_user_manager_unreachable", "controlling_user_process_unsignallable"} {
		if r, ok := by[n]; !ok || !r.Passed {
			t.Errorf("operator control %s: present=%v passed=%v (%s)", n, ok, r.Passed, r.Detail)
		}
	}
	wm, ok := by["controlling_worker_manager_unreachable"]
	if !ok {
		t.Fatal("controlling_worker_manager_unreachable did not run")
	}
	if wm.Passed {
		t.Errorf("controlling_worker_manager_unreachable passed although the worker's own bus is reachable: %s", wm.Detail)
	}
	if !strings.Contains(wm.Detail, fmt.Sprintf("uid=%d", f.workerUID)) {
		t.Errorf("worker manager detail does not name the worker uid %d: %s", f.workerUID, wm.Detail)
	}
	ws, ok := by["worker_manager_unsignallable"]
	if !ok {
		t.Fatal("worker_manager_unsignallable did not run")
	}
	if ws.Passed {
		t.Errorf("worker_manager_unsignallable passed although the worker can signal its own manager: %s", ws.Detail)
	}
	if len(f.pidAsked) != 1 || f.pidAsked[0] != f.workerUID {
		t.Errorf("manager pid looked up for uids %v, want exactly [%d] (the worker)", f.pidAsked, f.workerUID)
	}
	workerBus := fmt.Sprintf("/run/user/%d/bus", f.workerUID)
	probed := false
	for _, s := range f.scripts {
		if strings.Contains(s, workerBus) && strings.Contains(s, "systemd-run") {
			probed = true
		}
	}
	if !probed {
		t.Errorf("no probe targets the worker bus %s: %q", workerBus, f.scripts)
	}
	// The worker's sibling is stopped in the worker's manager.
	if !strings.Contains(strings.Join(f.stops, " "), "worker=true:fab-nc-wsibling-") {
		t.Errorf("worker sibling not stopped through the worker's manager: %v", f.stops)
	}
	if containment, failed := Capabilities(rs); containment {
		t.Errorf("Capabilities reports containment with the worker's own manager reachable (failed=%v)", failed)
	}
}

// The worker controls pass only on a positive denial, and every failure to
// look something up fails the control rather than skipping it.
func TestWorkerManagerControlsFailClosed(t *testing.T) {
	op := os.Getuid()
	g := controlGrant(t.TempDir())
	cases := []struct {
		name       string
		f          *fakeManagerHost
		wantBus    bool
		wantSignal bool
	}{
		{"boundary holds", &fakeManagerHost{workerUID: op + 5, workerMgrPID: 4242}, true, true},
		{"bus reachable only", &fakeManagerHost{workerUID: op + 5, workerMgrPID: 4242, workerBusOpen: true}, false, true},
		{"uid lookup fails", &fakeManagerHost{workerUID: op + 5, uidErr: errors.New("no such user")}, false, false},
		{"manager pid not found", &fakeManagerHost{workerUID: op + 5, pidErr: errors.New("pgrep: none")}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &Linux{User: "fab-worker", ctl: c.f.hooks()}
			rs, err := l.managerControls(g)
			if err != nil {
				t.Fatal(err)
			}
			by := resultsByName(rs)
			bus, ok1 := by["controlling_worker_manager_unreachable"]
			sig, ok2 := by["worker_manager_unsignallable"]
			if !ok1 || !ok2 {
				t.Fatalf("worker controls missing (bus=%v signal=%v): %+v", ok1, ok2, rs)
			}
			if bus.Passed != c.wantBus || sig.Passed != c.wantSignal {
				t.Errorf("bus passed=%v want %v (%s); signal passed=%v want %v (%s)", bus.Passed, c.wantBus, bus.Detail, sig.Passed, c.wantSignal, sig.Detail)
			}
		})
	}
	t.Run("an inconclusive connect is not a pass", func(t *testing.T) {
		f := &fakeManagerHost{workerUID: op + 5, workerMgrPID: 4242}
		h := f.hooks()
		inner := h.run
		h.run = func(gr Grant, s string) (string, int, error) {
			if strings.Contains(s, "socket.AF_UNIX") {
				return "bus=inconclusive:2\nunit=refused", 0, nil // ENOENT: no socket, systemd-run missing
			}
			return inner(gr, s)
		}
		rs, err := (&Linux{User: "fab-worker", ctl: h}).managerControls(g)
		if err != nil {
			t.Fatal(err)
		}
		if r := resultsByName(rs)["controlling_worker_manager_unreachable"]; r.Passed {
			t.Errorf("a missing socket passed the worker manager control: %s", r.Detail)
		}
	})
	t.Run("no worker account adds no worker controls", func(t *testing.T) {
		f := &fakeManagerHost{workerUID: op + 5, workerMgrPID: 4242}
		rs, err := (&Linux{ctl: f.hooks()}).managerControls(g)
		if err != nil {
			t.Fatal(err)
		}
		by := resultsByName(rs)
		if _, ok := by["controlling_worker_manager_unreachable"]; ok {
			t.Error("worker control ran without a worker account")
		}
		if len(rs) != 2 {
			t.Errorf("want the two operator controls, got %d", len(rs))
		}
	})
}

// goodqa#15976 AC-1 (review M1): control 4 measures MemoryMax under the
// worker account the real runs use. A variant that drops User measures the
// operator and fails this test.
func TestMemoryControlRunsAsTheWorker(t *testing.T) {
	l := &Linux{Self: "/x/fab", ReadOnly: []string{"/ro"}, TCPPorts: []int{443}, User: "fab-worker", MemoryMax: "8G"}
	m := l.memoryControlProfile()
	if m.User != "fab-worker" || m.Self != l.Self || m.MemoryMax != "64M" || len(m.ReadOnly) != 1 || len(m.TCPPorts) != 1 {
		t.Fatalf("memory control profile = %+v; want the worker account, this binary and roots, MemoryMax 64M", m)
	}
}

// goodqa#15976 AC-1 (review M3): manager control 9 gets its targets from the
// controller (inside PrivatePIDs a pgrep finds nothing), probes the
// operator's manager AND the worker uid's own manager, and passes only on
// EPERM. The old in-sandbox `pgrep` form never names a pid in its probe and
// fails here; so does a variant that probes os.Getuid() for the worker.
func TestManagerSignalControlsTargetSuppliedPIDs(t *testing.T) {
	op := os.Getuid()
	wuid := op + 7001
	pids := map[int]int{op: 31337, wuid: 42424}
	g := controlGrant(t.TempDir())
	var asked []int
	lookup := func(uid int) (int, error) {
		asked = append(asked, uid)
		if p, ok := pids[uid]; ok {
			return p, nil
		}
		return 0, errors.New("none")
	}
	for _, c := range []struct {
		name      string
		answer    func(script string) string
		wantPass  bool
		wantNames int
	}{
		{"filter denies both", func(string) string { return "signal=denied" }, true, 2},
		{"worker manager signallable", func(s string) string {
			if strings.Contains(s, " 42424") {
				return "signal=allowed"
			}
			return "signal=denied"
		}, false, 2},
		{"python crash is not a pass", func(string) string { return "ValueError: invalid literal for int()" }, false, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked = nil
			var scripts []string
			run := func(_ Grant, s string) (string, int, error) {
				scripts = append(scripts, s)
				return c.answer(s), 0, nil
			}
			rs := (&LinuxManager{User: "fab-worker"}).signalControls(g, wuid, lookup, run)
			if len(rs) != c.wantNames {
				t.Fatalf("got %d results, want %d: %+v", len(rs), c.wantNames, rs)
			}
			by := resultsByName(rs)
			w, ok := by["worker_manager_unsignallable"]
			if !ok {
				t.Fatal("worker_manager_unsignallable missing")
			}
			if w.Passed != c.wantPass {
				t.Errorf("worker control passed=%v want %v: %s", w.Passed, c.wantPass, w.Detail)
			}
			if len(asked) != 2 || asked[0] != op || asked[1] != wuid {
				t.Errorf("pids looked up for %v, want [%d %d]", asked, op, wuid)
			}
			joined := strings.Join(scripts, "\n")
			for _, pid := range []string{" 31337", " 42424"} {
				if !strings.Contains(joined, pid) {
					t.Errorf("no probe targets pid%s: %q", pid, scripts)
				}
			}
		})
	}
	t.Run("no pid found fails closed", func(t *testing.T) {
		rs := (&LinuxManager{User: "fab-worker"}).signalControls(g, wuid+1, lookup, func(Grant, string) (string, int, error) { return "signal=denied", 0, nil })
		if r := resultsByName(rs)["worker_manager_unsignallable"]; r.Passed {
			t.Errorf("a missing worker manager passed: %s", r.Detail)
		}
	})
}
