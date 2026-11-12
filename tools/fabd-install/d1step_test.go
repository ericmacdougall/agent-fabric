package fabdinstall_test

// goodqa#15373 review r1-pr56 F1/F5: d1-step.sh must restart the unit it
// stopped on every exit path. These tests run the real script with fake
// systemctl, systemd-run, fab, pgcall.py and sleep first on PATH, a /proc
// stand-in (FABD_D1_TEST_PROC) and the repo's recorded fake as the delegate
// source (FABD_D1_TEST_FAKE_SRC). Both hooks are accepted only with HOME under
// /tmp, so the tests skip elsewhere.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type d1Env struct {
	root, home, bin, state string
	env                    []string
}

const d1Unit = "fabd-t"
const d1Mission = "m_d1rh_test"

func newD1Env(t *testing.T, mode string) *d1Env {
	t.Helper()
	needBash(t)
	if runtime.GOOS != "linux" {
		t.Skip("d1-step.sh is Linux-only (getent, systemd)")
	}
	for _, tool := range []string{"jq", "sha256sum", "python3", "getent", "install"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	root := t.TempDir()
	if !strings.HasPrefix(root, "/tmp/") {
		t.Skipf("d1-step test hooks need t.TempDir under /tmp; got %s", root)
	}
	e := &d1Env{root: root, home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin"), state: filepath.Join(root, "state")}
	work := filepath.Join(e.home, "work")
	att := "r_d1rh_test_1"
	for _, d := range []string{e.bin, e.state, filepath.Join(e.home, "fabric-dev"), filepath.Join(e.home, ".config/systemd/user"),
		filepath.Join(e.home, "svc"), filepath.Join(root, "proc/4242"), filepath.Join(work, "run", att)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.home, "fabric-dev/test.env"), "FABRIC_TEST_DSN=postgres://t@127.0.0.1:1/x\n", 0o600)
	write(filepath.Join(e.home, ".config/systemd/user", d1Unit+".service"), "[Service]\nExecStart=/bin/true\n", 0o644)
	argv := []string{"/usr/bin/flock", "-n", e.home + "/svc/fabd.lock", e.bin + "/fab", "daemon", "--no-dispatch", "--provider", "/x/fakeclaude-live",
		"--trust-provider", "--work-root", work, "--tier-table", "t.json", "--rung", "R0", "--host", "t2-dev"}
	write(filepath.Join(root, "proc/4242/cmdline"), strings.Join(argv, "\x00")+"\x00", 0o644)
	write(filepath.Join(work, "run", att, "stream.jsonl"),
		`{"type":"system","subtype":"init","claude_code_version":"fake-1"}`+"\n"+
			`{"type":"result","structured_output":{"end":{"kind":"blocked","reason":"scripted"}}}`+"\n", 0o644)
	st := e.state
	// systemctl: units are "active" while state/active-<unit> exists; every call is logged
	writeExe(t, e.bin, "systemctl", `
echo "systemctl $*" >> `+st+`/calls
args=(); for a in "$@"; do [[ "$a" == --user || "$a" == --quiet || "$a" == --value ]] || args+=("$a"); done
set -- "${args[@]}"
case "$1" in
  --version) echo "systemd 255 (test)";;
  is-active) [[ -e `+st+`/active-$2 ]] && { echo active; exit 0; } || { echo inactive; exit 3; };;
  is-failed) echo inactive; exit 1;;
  cat) [[ "$2" == *-health.service ]] && exit 1; echo "# /home/u/.config/systemd/user/$2.service";;
  show) case "$3" in MainPID) echo 4242;; NRestarts) echo 0;; *) echo;; esac;;
  stop) rm -f `+st+`/active-$2;;
  start) touch `+st+`/active-$2;;
  reset-failed) :;;
esac
exit 0`)
	writeExe(t, e.bin, "systemd-run", `
echo "systemd-run $*" >> `+st+`/calls
[[ "$D1T_MODE" == runfail ]] && { echo "Failed to start transient service unit: Unit fabd-d1-step.service already exists." >&2; exit 1; }
touch `+st+`/active-fabd-d1-step; exit 0`)
	writeExe(t, e.bin, "pgrep", `exit 1`)
	// sleep: in mode term, the first poll sleep signals the script (its parent)
	writeExe(t, e.bin, "sleep", `
if [[ "$D1T_MODE" == term && ! -e `+st+`/termed ]]; then touch `+st+`/termed; kill -TERM $PPID; fi
exit 0`)
	writeExe(t, e.bin, "fab", `
echo "fab $*" >> `+st+`/calls
case "$1 $2" in
  "job dispatch-candidates") [[ "$3" == --only-mission ]] && echo "$4" || echo "$D1T_ALL";;
esac
exit 0`)
	// pgcall.py stand-in: canned answers; mode dbfail fails every read after the stop
	write(filepath.Join(e.home, "fabric-dev/pgcall.py"), `import os, sys
q = sys.argv[-1]
st = "`+st+`"
calls = open(os.path.join(st, "calls")).read() if os.path.exists(os.path.join(st, "calls")) else ""
stopped = "systemctl --user stop `+d1Unit+`" in calls
if os.environ.get("D1T_MODE") == "dbfail" and stopped and "systemctl --user start `+d1Unit+`" not in calls:
    sys.stderr.write("psql: connection refused\n"); sys.exit(2)
if "clock_timestamp" in q: print("2026-11-12T00:00:00.000000Z")
elif "sched_counters" in q: print("4")
elif "finish:%" in q: print("1" if os.environ.get("D1T_MODE") == "ok" else "0")
elif "SELECT id FROM attempts" in q: print("`+att+`" if os.environ.get("D1T_MODE") == "ok" else "")
elif "SELECT state FROM attempts" in q: print("VERIFIED" if os.environ.get("D1T_MODE") == "ok" else "")
elif "kind IN" in q: print("0")
else: print("")
`, 0o644)
	e.env = []string{"HOME=" + e.home, "PATH=" + e.bin + ":/usr/bin:/bin", "DB=fabric_t", "D1T_MODE=" + mode, "D1T_ALL=" + d1Mission,
		"D1_POLL_S=15", "D1_QUIET_S=0", "FABD_D1_TEST_PROC=" + filepath.Join(root, "proc"),
		"FABD_D1_TEST_FAKE_SRC=" + abs(t, "../fakeclaude/fakeclaude.sh")}
	return e
}

func (e *d1Env) run(t *testing.T) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", abs(t, "d1-step.sh"), d1Unit, e.home+"/svc", e.bin+"/fab", d1Mission, e.home+"/.local/share/fabric/d1")
	cmd.Env = e.env
	if err := os.WriteFile(filepath.Join(e.state, "active-"+d1Unit), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (e *d1Env) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.state, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// index of the first call equal to want after position from, or -1
func callAfter(calls []string, from int, want string) int {
	for i := from + 1; i < len(calls); i++ {
		if calls[i] == want {
			return i
		}
	}
	return -1
}

func TestD1StepRestoresOnEveryExitAfterStop(t *testing.T) {
	stop, start := "systemctl --user stop "+d1Unit, "systemctl --user start "+d1Unit
	// runfail (systemd-run refuses) and term (a signal while polling) abort the
	// script: the exit guard must restart the unit. dbfail (every database read
	// fails after the stop) must not abort before step 7, which restarts it.
	for _, tc := range []struct {
		mode   string
		onExit bool
	}{{"runfail", true}, {"term", true}, {"dbfail", false}} {
		mode := tc.mode
		t.Run(mode, func(t *testing.T) {
			e := newD1Env(t, mode)
			out, code := e.run(t)
			if code != 3 {
				t.Fatalf("exit %d, want 3:\n%s", code, out)
			}
			calls := e.calls(t)
			i := callAfter(calls, -1, stop)
			if i < 0 {
				t.Fatalf("the unit was never stopped; calls:\n%s\n%s", strings.Join(calls, "\n"), out)
			}
			if callAfter(calls, i, start) < 0 {
				t.Fatalf("the unit was stopped and never started again (availability, r1-pr56 F1); calls:\n%s\n%s", strings.Join(calls, "\n"), out)
			}
			if _, err := os.Stat(filepath.Join(e.state, "active-"+d1Unit)); err != nil {
				t.Errorf("the unit is not active at exit")
			}
			if !strings.Contains(out, "D1STEP result=FAIL") {
				t.Errorf("no FAIL result:\n%s", out)
			}
			if got := strings.Contains(out, "D1STEP restore=on-exit"); got != tc.onExit {
				t.Errorf("restore=on-exit recorded=%v, want %v:\n%s", got, tc.onExit, out)
			}
			if !tc.onExit && !strings.Contains(out, "D1STEP step=7 ") {
				t.Errorf("a database failure aborted the step before the step-7 restore:\n%s", out)
			}
		})
	}
}

// The guard is armed only after the stop: a refusal before it starts nothing.
func TestD1StepGateRefusalTouchesNothing(t *testing.T) {
	e := newD1Env(t, "ok")
	// the only-mission gate answers another mission id
	writeExe(t, e.bin, "fab", `
echo "fab $*" >> `+e.state+`/calls
[[ "$1 $2" == "job dispatch-candidates" ]] && echo m_other
exit 0`)
	out, code := e.run(t)
	if code != 3 || !strings.Contains(out, "dispatchable set is not exactly") {
		t.Fatalf("exit %d, want the gate refusal:\n%s", code, out)
	}
	for _, c := range e.calls(t) {
		if strings.HasPrefix(c, "systemctl --user stop "+d1Unit) || strings.HasPrefix(c, "systemctl --user start "+d1Unit) || strings.HasPrefix(c, "systemd-run") {
			t.Fatalf("a gate refusal must not stop, start or launch anything; saw %q", c)
		}
	}
}

// The success path restarts the unit exactly once and passes.
func TestD1StepSuccessRestartsOnce(t *testing.T) {
	e := newD1Env(t, "ok")
	out, code := e.run(t)
	if code != 0 || !strings.Contains(out, "D1STEP result=PASS") {
		t.Fatalf("exit %d, want PASS:\n%s", code, out)
	}
	n := 0
	for _, c := range e.calls(t) {
		if c == "systemctl --user start "+d1Unit {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the unit was started %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "restore=on-exit") {
		t.Errorf("the exit guard fired on the success path:\n%s", out)
	}
	if !strings.Contains(strings.Join(e.calls(t), "\n"), "systemd-run --user --quiet --collect --unit fabd-d1-step") {
		t.Errorf("the transient is not started with --collect")
	}
}
