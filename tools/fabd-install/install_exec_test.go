// Behavioural tests of install.sh itself (goodqa#15805 review B4): the installer
// runs for real against stub systemctl, systemd-analyze, docker and fab commands
// with a temporary HOME. They replace source-text checks that a reordered or
// weakened installer could still pass.
package fabdinstall_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type installEnv struct {
	home, bin, work, unitDir, svc string
	fab, fake, emit, healthState  string
	docker                        fakeDocker
	env                           []string
}

const goodShow = "Restart=always\nRestartUSec=10s\nStartLimitIntervalUSec=0\n"

// newInstall builds a temporary HOME holding an existing live install: the
// service directory, the installed unit (rendered with the same FAB_BIN and
// provider, as on t2-dev) and the hand-made 10-retry.conf drop-in.
func newInstall(t *testing.T, policy string) *installEnv {
	t.Helper()
	root := t.TempDir()
	e := &installEnv{home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin"), work: filepath.Join(root, "evidence")}
	e.unitDir = filepath.Join(e.home, ".config/systemd/user")
	e.svc = filepath.Join(e.home, "fabd")
	e.healthState = filepath.Join(e.home, ".local/state/fabd-health")
	for _, d := range []string{e.bin, e.unitDir + "/fabd.service.d", e.svc, e.healthState} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.fab = filepath.Join(e.bin, "fab-live")
	writeExe(t, e.bin, "fab-live", `[[ "$1" == version ]] && echo "fab bd56178ed865"; exit 0`)
	e.fake = filepath.Join(e.bin, "fakeclaude")
	writeExe(t, e.bin, "fakeclaude", "exit 0\n")
	e.emit = filepath.Join(e.bin, "ops_watch.py")
	writeExe(t, e.bin, "ops_watch.py", `[[ -e "`+root+`/emit-fails" ]] && exit 1; printf '%s\n' "$3" >> "`+root+`/emits.jsonl"; echo 1`)
	os.WriteFile(filepath.Join(e.svc, "env"), []byte("FABRIC_DSN=postgres://x@127.0.0.1/fabric_live\n"), 0o600)
	writeExe(t, e.svc, "guard.sh", "exit 0\n")
	// systemctl: every call is logged; show answers from the file "show";
	// starting the health service writes the state file from the file "health"
	// (as the real oneshot would), default "0 0 0" (a passing check).
	writeExe(t, e.bin, "systemctl", `
printf '%s\n' "$*" >> "`+root+`/systemctl.log"
shift  # --user
case "$1" in
  is-enabled) echo enabled;;
  is-active) echo active;;
  show) cat "`+root+`/show" 2>/dev/null || printf '`+strings.ReplaceAll(goodShow, "\n", `\n`)+`';;
  start)
    if [[ "$2" == fabd-health.service ]]; then
      mkdir -p "`+e.healthState+`"
      cat "`+root+`/health" 2>/dev/null > "`+e.healthState+`/fabd.state" || true
      [[ -s "`+e.healthState+`/fabd.state" ]] || echo "0 0 0" > "`+e.healthState+`/fabd.state"
    fi;;
esac
exit 0`)
	writeExe(t, e.bin, "systemd-analyze", "exit 0\n")
	e.docker = newFakeDocker(t, policy)
	// the installed unit, rendered exactly as the installer would render it
	out, err := exec.Command("bash", "-c", `. "$1"; render_unit live "$2" "$3" "$4" t2-dev`, "x", abs(t, "unit.sh"), e.svc, e.fab, e.fake).Output()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.unitDir, "fabd.service"), append([]byte("# installed copy\n"), out...), 0o644)
	os.WriteFile(filepath.Join(e.unitDir, "fabd.service.d/10-retry.conf"), []byte("[Service]\nRestart=always\n"), 0o644)
	e.env = []string{
		"HOME=" + e.home,
		"TMPDIR=" + root,
		"PATH=" + e.bin + ":" + e.docker.dir + ":" + os.Getenv("PATH"),
		"MODE=unit", "FAB_BIN=" + e.fab, "FABD_INSTALL_TEST_FAKE=" + e.fake,
		"FABD_HEALTH_EMIT=" + e.emit, "WORKDIR=" + e.work,
		"XDG_RUNTIME_DIR=" + root,
	}
	return e
}

func (e *installEnv) root() string { return filepath.Dir(e.home) }

func (e *installEnv) run(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	_, se, code := runBash(t, append(append([]string{}, e.env...), extra...), `exec bash "$1"`, abs(t, "install.sh"))
	log, _ := os.ReadFile(filepath.Join(e.work, "install.log"))
	return se + string(log), code
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// AC-7 with review B1, B2, F2, N5, N9, N13: MODE=unit upgrades the unit, keeps
// the previous unit and the drop-in, asserts the effective settings and reads
// the health check's real result.
func TestInstallUnitMode(t *testing.T) {
	needBash(t)
	t.Run("upgrade", func(t *testing.T) {
		e := newInstall(t, "unless-stopped")
		before := read(t, filepath.Join(e.unitDir, "fabd.service"))
		log, code := e.run(t)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, log)
		}
		if got := read(t, filepath.Join(e.work, "prev-fabd.service")); got != before {
			t.Errorf("the previous unit was not kept for rollback (B2): %q", got)
		}
		if got := read(t, filepath.Join(e.work, "dropin-10-retry.conf")); got != "[Service]\nRestart=always\n" {
			t.Errorf("the drop-in was not kept before removal: %q", got)
		}
		if _, err := os.Stat(filepath.Join(e.unitDir, "fabd.service.d/10-retry.conf")); !os.IsNotExist(err) {
			t.Errorf("the drop-in is still installed")
		}
		if !strings.Contains(e.docker.calls(t), "update --restart always fabric-pg") {
			t.Errorf("MODE=unit did not move fabric-pg to always (N9):\n%s", e.docker.calls(t))
		}
		hs := read(t, filepath.Join(e.unitDir, "fabd-health.service"))
		if !strings.Contains(hs, "Environment=FABD_HEALTH_EMIT="+e.emit) || !strings.Contains(hs, "Environment=FABD_HEALTH_STATE_DIR="+e.healthState) {
			t.Errorf("the health service does not get the emitter and state dir the installer checked (N13):\n%s", hs)
		}
		sc := read(t, filepath.Join(e.root(), "systemctl.log"))
		if !strings.Contains(sc, "enable --now fabd-health.timer") {
			t.Errorf("the health timer was not enabled:\n%s", sc)
		}
		for _, never := range []string{"restart fabd", "stop fabd.service", "start fabd.service"} {
			if strings.Contains(sc, never) {
				t.Errorf("MODE=unit ran %q", never)
			}
		}
		if !strings.Contains(log, "health check of fabd: pass") {
			t.Errorf("no health pass line:\n%s", log)
		}
	})
	t.Run("failed first health check stops the install B1", func(t *testing.T) {
		e := newInstall(t, "always")
		os.WriteFile(filepath.Join(e.root(), "health"), []byte("1 0 0\n"), 0o644)
		log, code := e.run(t)
		if code != 3 || strings.Contains(log, "health check of fabd: pass") {
			t.Errorf("a failed first check must stop with exit 3, got %d:\n%s", code, log)
		}
	})
	t.Run("effective settings that differ stop the install F2", func(t *testing.T) {
		e := newInstall(t, "always")
		os.WriteFile(filepath.Join(e.root(), "show"), []byte("Restart=on-failure\nRestartUSec=10s\nStartLimitIntervalUSec=0\n"), 0o644)
		log, code := e.run(t)
		if code != 3 || !strings.Contains(log, "effective unit settings differ") {
			t.Errorf("want exit 3 on effective Restart=on-failure, got %d:\n%s", code, log)
		}
	})
	t.Run("a changed FAB_BIN is refused before any change N5", func(t *testing.T) {
		e := newInstall(t, "always")
		before := read(t, filepath.Join(e.unitDir, "fabd.service"))
		other := filepath.Join(e.bin, "fab-other")
		writeExe(t, e.bin, "fab-other", `[[ "$1" == version ]] && echo "fab other"; exit 0`)
		log, code := e.run(t, "FAB_BIN="+other)
		if code != 3 || !strings.Contains(log, "would change ExecStart") {
			t.Errorf("want exit 3 for a binary change, got %d:\n%s", code, log)
		}
		if read(t, filepath.Join(e.unitDir, "fabd.service")) != before {
			t.Errorf("the unit changed although the run was refused")
		}
	})
}

// AC-3, NEG-6 with review B4: a failed preflight changes nothing.
func TestInstallPreflightChangesNothing(t *testing.T) {
	needBash(t)
	cases := map[string]func(e *installEnv) []string{
		"restart policy no": func(e *installEnv) []string { return nil },
		"no emitter":        func(e *installEnv) []string { return []string{"FABD_HEALTH_EMIT=" + filepath.Join(e.bin, "absent.py")} },
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			policy := "always"
			if name == "restart policy no" {
				policy = "no"
			}
			e := newInstall(t, policy)
			before := read(t, filepath.Join(e.unitDir, "fabd.service"))
			log, code := e.run(t, extra(e)...)
			if code != 3 {
				t.Errorf("want exit 3, got %d:\n%s", code, log)
			}
			if read(t, filepath.Join(e.unitDir, "fabd.service")) != before {
				t.Errorf("the unit changed after a failed preflight")
			}
			if _, err := os.Stat(filepath.Join(e.unitDir, "fabd.service.d/10-retry.conf")); err != nil {
				t.Errorf("the drop-in was touched after a failed preflight")
			}
			if strings.Contains(e.docker.calls(t), "update") {
				t.Errorf("docker update ran after a failed preflight")
			}
			if sc := read(t, filepath.Join(e.root(), "systemctl.log")); strings.Contains(sc, "daemon-reload") || strings.Contains(sc, "enable") {
				t.Errorf("systemctl changed state after a failed preflight:\n%s", sc)
			}
		})
	}
}

// The test hook cannot apply to a real install: with a HOME outside the temp
// directory it is refused.
func TestInstallTestHookNeedsTempHome(t *testing.T) {
	needBash(t)
	e := newInstall(t, "always")
	log, code := e.run(t, "TMPDIR=/nonexistent-tmp", "HOME=/home/someone")
	if code != 2 || !strings.Contains(log, "temporary HOME only") {
		t.Errorf("FABD_INSTALL_TEST_FAKE with a real-looking HOME: want the hook refusal (exit 2), got %d:\n%s", code, log)
	}
}

// AC-3 / review F3, N7: removal stops the timer and clears a raised alert; a
// failed clear stops the removal with the health script and units kept.
func TestInstallRemoveClearsAlert(t *testing.T) {
	needBash(t)
	setup := func(t *testing.T) *installEnv {
		e := newInstall(t, "always")
		d := filepath.Join(e.home, "fabric-dev")
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "test.env"), []byte("FABRIC_TEST_DSN=postgres://admin@127.0.0.1:55990/postgres\n"), 0o600)
		// pgcall stand-in: counts answer 0, everything else is empty
		os.WriteFile(filepath.Join(d, "pgcall.py"), []byte("import sys\nq = ' '.join(sys.argv)\nprint('0' if 'count(' in q else '')\n"), 0o600)
		os.WriteFile(filepath.Join(e.svc, "old_scheduler_role"), nil, 0o600)
		hs := read(t, abs(t, "fabd-health.sh"))
		os.WriteFile(filepath.Join(e.svc, "fabd-health.sh"), []byte(hs), 0o755)
		os.WriteFile(filepath.Join(e.healthState, "fabd.state"), []byte("3 1 0\n"), 0o600)
		for _, u := range []string{"fabd-health.service", "fabd-health.timer"} {
			os.WriteFile(filepath.Join(e.unitDir, u), []byte("[Unit]\n"), 0o644)
		}
		return e
	}
	t.Run("clear", func(t *testing.T) {
		e := setup(t)
		log, code := e.run(t, "MODE=remove", "FABD_HEALTH_HOST=t2-dev")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, log)
		}
		if ev := read(t, filepath.Join(e.root(), "emits.jsonl")); !strings.Contains(ev, `"event": "clear"`) || !strings.Contains(ev, "fabric:live-down:fabd") {
			t.Errorf("no clear emitted: %q", ev)
		}
		if sc := read(t, filepath.Join(e.root(), "systemctl.log")); !strings.Contains(sc, "disable --now fabd-health.timer") {
			t.Errorf("the timer was not disabled:\n%s", sc)
		}
		if _, err := os.Stat(filepath.Join(e.unitDir, "fabd-health.timer")); !os.IsNotExist(err) {
			t.Errorf("the health timer file is still installed")
		}
	})
	t.Run("failed clear stops the removal N7", func(t *testing.T) {
		e := setup(t)
		os.WriteFile(filepath.Join(e.root(), "emit-fails"), nil, 0o644)
		log, code := e.run(t, "MODE=remove", "FABD_HEALTH_HOST=t2-dev")
		if code != 3 || !strings.Contains(log, "could not clear") {
			t.Errorf("want exit 3 on a failed clear, got %d:\n%s", code, log)
		}
		if _, err := os.Stat(filepath.Join(e.unitDir, "fabd-health.service")); err != nil {
			t.Errorf("the health service was removed although the clear failed")
		}
		if sc := read(t, filepath.Join(e.root(), "systemctl.log")); strings.Contains(sc, "stop fabd.service") {
			t.Errorf("fabd was stopped although the clear failed")
		}
	})
}

func (h *healthEnv) writeState(t *testing.T, s string) {
	t.Helper()
	os.MkdirAll(h.state, 0o700)
	os.WriteFile(filepath.Join(h.state, "fabd.state"), []byte(s), 0o600)
}

// Review N3: a standing raise is re-emitted (reasserted) once the interval has
// passed, so a bounded ring cannot lose it; not before.
func TestHealthReassertsStandingRaise(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	h.env = append(h.env, "FABD_HEALTH_REASSERT_S=3600")
	h.set(t, false, false)
	h.writeState(t, "5 1 "+strconv.FormatInt(time.Now().Unix()-60, 10)+"\n")
	h.run(t)
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("re-emitted before the interval: %v", ev)
	}
	h.writeState(t, "5 1 "+strconv.FormatInt(time.Now().Unix()-7200, 10)+"\n")
	h.run(t)
	ev := h.events(t)
	if len(ev) != 1 || ev[0]["event"] != "raise" || ev[0]["severity"] != "critical" {
		t.Fatalf("want one reasserted critical raise, got %v", ev)
	}
	if d, _ := ev[0]["detail"].(map[string]any); d["reasserted"] != true {
		t.Errorf("the re-emitted raise is not marked reasserted: %v", ev[0])
	}
}

// Review N8: disabling a unit with a standing raise clears it.
func TestHealthDisabledClearsStandingRaise(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	os.WriteFile(filepath.Join(h.dir, "enabled"), []byte("disabled\n"), 0o644)
	h.writeState(t, "3 1 0\n")
	if _, se, code := h.run(t); code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if ev := h.events(t); len(ev) != 1 || ev[0]["event"] != "clear" {
		t.Errorf("want one clear for a disabled unit with a standing raise, got %v", ev)
	}
}

// Review N4: the failure is counted before the probe, so a probe that hangs
// until systemd kills the check still counts; the probe gets connect_timeout.
func TestHealthCountsBeforeProbe(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	h.set(t, true, true)
	h.writeState(t, "1 0 0\n")
	writeExe(t, h.dir, "fab", `cat "`+h.state+`/fabd.state" > "`+h.dir+`/state-during-probe"; echo "$FABRIC_DSN" > "`+h.dir+`/seen-dsn"; cat "`+h.dir+`/metrics"`)
	if _, se, code := h.run(t); code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if got := read(t, filepath.Join(h.dir, "state-during-probe")); !strings.HasPrefix(got, "2 ") {
		t.Errorf("state during the probe = %q, want the failure already counted (2)", got)
	}
	if got := read(t, filepath.Join(h.state, "fabd.state")); !strings.HasPrefix(got, "0 ") {
		t.Errorf("a passing probe must reset the count, state %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(h.dir, "seen-dsn")), "connect_timeout=10") {
		t.Errorf("the probe DSN has no connect_timeout")
	}
}

// Review N14: a maintenance window starts a fresh count.
func TestHealthMaintenanceResetsCount(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	h.writeState(t, "1 0 0\n")
	runBash(t, h.env, `exec "$1" --maintenance fabd 5`, abs(t, "fabd-health.sh"))
	h.set(t, false, false)
	h.run(t)
	if got := read(t, filepath.Join(h.state, "fabd.state")); !strings.HasPrefix(got, "0 ") {
		t.Errorf("state after a skipped check in a window = %q, want 0", got)
	}
}
