// Tests for the fabd installer pieces (goodqa#15805): the rendered unit, the
// fabric-pg restart-policy helper, the installer preflight order and the
// health check that raises an ops-watch alert when fabd stays down.
//
// The scripts are bash; these tests run them with fake docker, systemctl, fab
// and emitter commands placed first on PATH. They need bash 4+ (Linux); on an
// older bash they skip.
package fabdinstall_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func needBash(t *testing.T) {
	t.Helper()
	out, err := exec.Command("bash", "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if v, convErr := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || convErr != nil || v < 4 {
		t.Skip("bash 4+ required")
	}
}

func writeExe(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runBash(t *testing.T, env []string, script string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", script, "test"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

func abs(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// section returns the lines of one [Section] of a unit file.
func section(unit, name string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(unit, "\n") {
		if strings.HasPrefix(ln, "[") {
			in = ln == "["+name+"]"
			continue
		}
		if in {
			b.WriteString(ln + "\n")
		}
	}
	return b.String()
}

// AC-1: the unit keeps retrying after a reboot until its database is up.
func TestUnitRetriesWithoutStartLimit(t *testing.T) {
	needBash(t)
	out, se, code := runBash(t, nil, `source "$1"; render_unit live /home/u/fabd /srv/fabric/bin/fab-live /srv/fabric/bin/fakeclaude-live t2-dev`, abs(t, "unit.sh"))
	if code != 0 {
		t.Fatalf("render_unit exit %d: %s", code, se)
	}
	unitSec, svcSec := section(out, "Unit"), section(out, "Service")
	for _, want := range []string{"StartLimitIntervalSec=0\n"} {
		if !strings.Contains(unitSec, want) {
			t.Errorf("[Unit] lacks %q:\n%s", want, unitSec)
		}
	}
	for _, want := range []string{"Restart=always\n", "RestartSec=10\n", "EnvironmentFile=/home/u/fabd/env\n", "ExecStartPre=/home/u/fabd/guard.sh\n"} {
		if !strings.Contains(svcSec, want) {
			t.Errorf("[Service] lacks %q:\n%s", want, svcSec)
		}
	}
	if regexp.MustCompile(`(?m)^StartLimitBurst=`).MatchString(out) {
		t.Errorf("unit still sets StartLimitBurst:\n%s", out)
	}
	if !strings.Contains(out, "ExecStart=/usr/bin/flock -n /home/u/fabd/fabd.lock /srv/fabric/bin/fab-live daemon --no-dispatch --provider /srv/fabric/bin/fakeclaude-live --trust-provider --work-root /home/u/fabd/work --tier-table /home/u/fabd/tiers.json --rung R0 --host t2-dev\n") {
		t.Errorf("ExecStart changed:\n%s", out)
	}
}

// AC-4: the health units run the check every 2 minutes with fabd's environment.
func TestHealthUnits(t *testing.T) {
	needBash(t)
	out, se, code := runBash(t, nil, `source "$1"; render_health_service fabd /home/u/fabd /srv/fabric/bin/fab-live /opt/fabd-health.sh; echo ===; render_health_timer fabd`, abs(t, "unit.sh"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	svc, timer, _ := strings.Cut(out, "===\n")
	for _, want := range []string{"Type=oneshot\n", "EnvironmentFile=/home/u/fabd/env\n", "ExecStart=/opt/fabd-health.sh fabd /srv/fabric/bin/fab-live\n"} {
		if !strings.Contains(svc, want) {
			t.Errorf("health service lacks %q:\n%s", want, svc)
		}
	}
	for _, want := range []string{"OnUnitActiveSec=2min\n", "OnBootSec=3min\n", "Unit=fabd-health.service\n", "WantedBy=timers.target\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("health timer lacks %q:\n%s", want, timer)
		}
	}
}

type fakeDocker struct {
	dir, log string
}

// newFakeDocker answers `docker inspect` with the given restart policy, or
// "missing" for no container, and records every argv line and its environment.
func newFakeDocker(t *testing.T, policy string) fakeDocker {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	pf := filepath.Join(dir, "policy")
	if err := os.WriteFile(pf, []byte(policy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExe(t, dir, "docker", `
printf '%s\n' "$*" >> "`+log+`"
env | grep -i sentinel >> "`+log+`" || true
p=$(cat "`+pf+`")
case "$1" in
  inspect)
    [[ "$p" == missing ]] && { echo "Error: No such object" >&2; exit 1; }
    if [[ "$3" == *Running* ]]; then cat "`+dir+`/running" 2>/dev/null || echo "true 0"; else echo "$p"; fi;;
  exec) exit "$(cat "`+dir+`/exec-rc" 2>/dev/null || echo 0)";;
  update) [[ "$2" == --restart ]] && echo "$3" > "`+pf+`";;
  run) echo always > "`+pf+`";;
esac
exit 0
`)
	return fakeDocker{dir, log}
}

func (f fakeDocker) calls(t *testing.T) string {
	b, _ := os.ReadFile(f.log)
	return string(b)
}

// AC-2, NEG-2, NEG-3.
func TestFabricPG(t *testing.T) {
	needBash(t)
	script := abs(t, "fabric-pg.sh")
	pw := filepath.Join(t.TempDir(), "pg.pw")
	if err := os.WriteFile(pw, []byte("SENTINEL-PASSWORD-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, policy, cmd string
		wantCode          int // 0 or non-zero (any)
		mustCall, never   []string
	}{
		{"check ok unless-stopped", "unless-stopped", "check", 0, []string{"inspect"}, []string{"update", "run", "rm"}},
		{"check ok always", "always", "check", 0, nil, []string{"update", "run", "rm"}},
		{"check refuses no", "no", "check", 1, nil, []string{"update", "run", "rm"}},
		{"check refuses on-failure", "on-failure", "check", 1, nil, []string{"update", "run", "rm"}},
		{"check missing", "missing", "check", 1, nil, []string{"update", "run", "rm"}},
		{"ensure sets policy", "no", "ensure", 0, []string{"update --restart always fabric-pg"}, []string{"run", "rm", "volume"}},
		{"ensure missing never creates", "missing", "ensure", 1, nil, []string{"update", "run", "rm"}},
		{"create refuses existing", "no", "create", 1, nil, []string{"run", "rm", "update"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := newFakeDocker(t, c.policy)
			env := []string{"PATH=" + fd.dir + ":" + os.Getenv("PATH"), "FABRIC_PG_PASSWORD_FILE=" + pw}
			_, se, code := runBash(t, env, `exec "$1" "$2"`, script, c.cmd)
			if (code == 0) != (c.wantCode == 0) {
				t.Fatalf("exit %d, want %d; stderr: %s", code, c.wantCode, se)
			}
			calls := fd.calls(t)
			for _, m := range c.mustCall {
				if !strings.Contains(calls, m) {
					t.Errorf("docker never called with %q; calls:\n%s", m, calls)
				}
			}
			for _, n := range c.never {
				if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(n) + `\b`).MatchString(calls) {
					t.Errorf("docker called with %q; calls:\n%s", n, calls)
				}
			}
			if code != 0 && strings.TrimSpace(se) == "" {
				t.Errorf("a refusal must say why")
			}
		})
	}
	t.Run("create new container", func(t *testing.T) {
		fd := newFakeDocker(t, "missing")
		env := []string{"PATH=" + fd.dir + ":" + os.Getenv("PATH"), "FABRIC_PG_PASSWORD_FILE=" + pw}
		_, se, code := runBash(t, env, `exec "$1" create`, script)
		if code != 0 {
			t.Fatalf("create exit %d: %s", code, se)
		}
		calls := fd.calls(t)
		var run string
		for _, ln := range strings.Split(calls, "\n") {
			if strings.HasPrefix(ln, "run ") {
				run = ln
			}
		}
		for _, want := range []string{"--restart always", "--name fabric-pg", "-p 127.0.0.1:55990:5432", "-v fabric-pg-data:/var/lib/postgresql/data", "POSTGRES_PASSWORD_FILE=/run/secrets/fabric-pg.pw", pw + ":/run/secrets/fabric-pg.pw:ro"} {
			if !strings.Contains(run, want) {
				t.Errorf("docker run lacks %q: %s", want, run)
			}
		}
		if strings.Contains(calls, "SENTINEL-PASSWORD-VALUE") {
			t.Errorf("the password value reached docker argv or env:\n%s", calls)
		}
	})
	t.Run("create refuses a container that never becomes ready", func(t *testing.T) {
		fd := newFakeDocker(t, "missing")
		os.WriteFile(filepath.Join(fd.dir, "running"), []byte("true 3\n"), 0o644)
		env := []string{"PATH=" + fd.dir + ":" + os.Getenv("PATH"), "FABRIC_PG_PASSWORD_FILE=" + pw, "FABRIC_PG_READY_TRIES=2"}
		_, se, code := runBash(t, env, `exec "$1" create`, script)
		if code == 0 || !strings.Contains(se, "did not become ready") {
			t.Errorf("create reported success for a crash-looping container (exit %d): %s", code, se)
		}
	})
	t.Run("create needs a password file", func(t *testing.T) {
		fd := newFakeDocker(t, "missing")
		env := []string{"PATH=" + fd.dir + ":" + os.Getenv("PATH"), "FABRIC_PG_PASSWORD_FILE=" + filepath.Join(t.TempDir(), "absent")}
		_, _, code := runBash(t, env, `exec "$1" create`, script)
		if code == 0 || strings.Contains(fd.calls(t), "run ") {
			t.Errorf("create ran without a readable password file")
		}
	})
}

// AC-3, NEG-6: the installer checks fabric-pg before it writes or enables the unit.
func TestInstallPreflight(t *testing.T) {
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	check := strings.Index(s, `fabric-pg.sh" check`)
	unit := strings.Index(s, "phase_done unit")
	render := strings.Index(s, "render_unit ")
	if check < 0 {
		t.Fatal(`install.sh never runs fabric-pg.sh check`)
	}
	if unit < 0 || render < 0 {
		t.Fatal("install.sh unit phase or render_unit call not found")
	}
	if check > unit || check > render {
		t.Errorf("fabric-pg check (offset %d) must come before the unit phase (%d) and render_unit (%d)", check, unit, render)
	}
	// NEG-6: before anything that changes state or even reads the admin credential
	for _, later := range []string{`ADMIN=$(`, "CREATE DATABASE", "if ! phase_done snapshot", "if ! phase_done roles", "! phase_done copy"} {
		at := strings.Index(s, later)
		if at < 0 {
			t.Errorf("install.sh no longer has %q; update this test", later)
		} else if check > at {
			t.Errorf("fabric-pg check (offset %d) comes after %q (%d)", check, later, at)
		}
	}
	// MODE=unit (upgrade of an existing install) leaves before roles, data and the admin credential
	modeUnit := strings.Index(s, `if [[ $MODE == unit ]]; then`)
	if modeUnit < 0 {
		t.Fatal("install.sh has no MODE=unit path")
	}
	if admin := strings.Index(s, `ADMIN=$(`); modeUnit > admin {
		t.Errorf("the MODE=unit path must exit before the admin credential is read")
	}
	unitBlock := s[modeUnit:]
	unitBlock = unitBlock[:strings.Index(unitBlock, "\nfi\n")]
	for _, want := range []string{"write_unit_files", "run_health", "exit 0"} {
		if !strings.Contains(unitBlock, want) {
			t.Errorf("MODE=unit block lacks %q:\n%s", want, unitBlock)
		}
	}
	for _, never := range []string{"pq ", "pg_dump", "ROLE", "deploy"} {
		if strings.Contains(unitBlock, never) {
			t.Errorf("MODE=unit block touches %q", never)
		}
	}
	// the effective settings are asserted, and a hand-made drop-in is kept as a copy before removal
	for _, want := range []string{`systemctl --user show "$UNIT.service" -p Restart -p RestartUSec -p StartLimitIntervalUSec`, `cmp -s "$f"`} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	// removal stops the health timer and clears a raised alert (review F3)
	rm := s[strings.Index(s, "do_remove() {"):]
	rm = rm[:strings.Index(rm, "\n}\n")]
	for _, want := range []string{`disable --now "$UNIT-health.timer"`, `fabd-health.sh" --clear "$UNIT"`, `$UNIT-health.service"`} {
		if !strings.Contains(rm, want) {
			t.Errorf("do_remove lacks %q", want)
		}
	}
	if !regexp.MustCompile(`fabric-pg\.sh" check[^\n]*\|\|\s*die`).MatchString(s) {
		t.Errorf("a failed fabric-pg check must stop the install with die")
	}
	if strings.Contains(s, "StartLimitBurst") {
		t.Errorf("install.sh still writes StartLimitBurst itself; the unit must come from unit.sh")
	}
	if !strings.Contains(s, "fabd-health.timer") {
		t.Errorf("install.sh does not install the health timer")
	}
}

type healthEnv struct {
	dir, state, emits string
	env               []string
}

func newHealth(t *testing.T) *healthEnv {
	t.Helper()
	dir := t.TempDir()
	h := &healthEnv{dir: dir, state: filepath.Join(dir, "state"), emits: filepath.Join(dir, "emits.jsonl")}
	// systemctl answers from the file "active"; fab metrics from the file "metrics".
	// systemctl: is-enabled answers from the file "enabled" (default enabled),
	// is-active from the file "active".
	writeExe(t, dir, "systemctl", `
if [[ "$2" == is-enabled ]]; then
  e=$(cat "`+dir+`/enabled" 2>/dev/null || echo enabled); echo "$e"; [[ "$e" == enabled ]]; exit
fi
cat "`+dir+`/active" 2>/dev/null || echo inactive; [[ "$(cat "`+dir+`/active" 2>/dev/null)" == active ]]`)
	writeExe(t, dir, "fab", `[[ "$1" == metrics ]] || exit 2; echo "dsn=$FABRIC_DSN" >&2; echo "$FABRIC_DSN" > "`+dir+`/seen-dsn"; cat "`+dir+`/metrics" 2>/dev/null`)
	// the emitter fails while the file "emit-fails" exists
	writeExe(t, dir, "ops_watch.py", `[[ "$1" == emit && "$2" == --json ]] || exit 2; [[ -e "`+dir+`/emit-fails" ]] && exit 1; printf '%s\n' "$3" >> "`+h.emits+`"; echo 1`)
	writeExe(t, dir, "logger", `printf '%s\n' "$*" >> "`+dir+`/logger.log"`)
	h.env = []string{
		"PATH=" + dir + ":" + os.Getenv("PATH"),
		"FABD_HEALTH_STATE_DIR=" + h.state,
		"FABD_HEALTH_EMIT=" + filepath.Join(dir, "ops_watch.py"),
		"FABD_HEALTH_HOST=t2-dev",
		"FABRIC_DSN=postgres://SENTINEL-DSN@127.0.0.1/x",
	}
	return h
}

func (h *healthEnv) set(t *testing.T, active bool, metricsOK bool) {
	t.Helper()
	a, m := "inactive", "fabric_schema_level 38\n"
	if active {
		a = "active"
	}
	if metricsOK {
		m += "fabric_build_info{version=\"bd56178ed865\"} 1\n"
	}
	os.WriteFile(filepath.Join(h.dir, "active"), []byte(a+"\n"), 0o644)
	os.WriteFile(filepath.Join(h.dir, "metrics"), []byte(m), 0o644)
}

func (h *healthEnv) run(t *testing.T) (string, string, int) {
	t.Helper()
	return runBash(t, h.env, `exec "$1" fabd "$2"`, abs(t, "fabd-health.sh"), filepath.Join(h.dir, "fab"))
}

func (h *healthEnv) events(t *testing.T) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(h.emits)
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("emitted invalid JSON %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

// AC-4, NEG-4, NEG-5.
func TestHealthRaisesAfterTwoFailuresAndClears(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	step := func(active, metrics bool) (string, string, int) {
		h.set(t, active, metrics)
		return h.run(t)
	}
	if _, se, code := step(true, true); code != 0 || len(h.events(t)) != 0 {
		t.Fatalf("healthy run: exit %d, events %v, stderr %s", code, h.events(t), se)
	}
	// one blip, then good: nothing emitted
	step(false, false)
	step(true, true)
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("a single failed check emitted %v", ev)
	}
	// two consecutive failures: one critical raise
	step(false, false)
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("raised after one failure: %v", ev)
	}
	so, se, _ := step(false, false)
	ev := h.events(t)
	if len(ev) != 1 {
		t.Fatalf("want one raise after two failures, got %v", ev)
	}
	e := ev[0]
	if e["event"] != "raise" || e["severity"] != "critical" || e["key"] != "fabric:live-down:fabd" || e["host"] != "t2-dev" || e["source"] != "fabd-health" || e["message"] == "" {
		t.Errorf("bad raise event: %v", e)
	}
	// still down: no repeat
	step(false, false)
	step(true, false) // active but metrics missing is still a failure
	if ev := h.events(t); len(ev) != 1 {
		t.Fatalf("repeated raise while already raised: %v", ev)
	}
	// recovery: one clear
	step(true, true)
	step(true, true)
	ev = h.events(t)
	if len(ev) != 2 || ev[1]["event"] != "clear" || ev[1]["key"] != "fabric:live-down:fabd" {
		t.Fatalf("want one clear on recovery, got %v", ev)
	}
	all := so + se
	b, _ := os.ReadFile(h.emits)
	if strings.Contains(all, "SENTINEL-DSN") || strings.Contains(string(b), "SENTINEL-DSN") {
		t.Errorf("the DSN leaked into output or the alert")
	}
}

// NEG-4: no emitter means a visible failure, not a silent pass.
func TestHealthFailsLoudWithoutEmitter(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	h.env = append(h.env, "FABD_HEALTH_EMIT="+filepath.Join(h.dir, "absent-ops-watch.py"))
	h.set(t, false, false)
	h.run(t)
	_, se, code := h.run(t)
	if code == 0 {
		t.Errorf("health check passed with no emitter while fabd was down")
	}
	if !strings.Contains(se, "emit") {
		t.Errorf("stderr should name the emit failure: %q", se)
	}
}

// NEG-5: the metrics call is read-only.
func TestHealthMetricsReadOnly(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	writeExe(t, h.dir, "fab", `echo "$PGOPTIONS" > "`+h.dir+`/pgopts"; echo 'fabric_build_info{version="x"} 1'`)
	h.set(t, true, true)
	if _, se, code := h.run(t); code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, "pgopts"))
	if !strings.Contains(string(b), "default_transaction_read_only=on") {
		t.Errorf("fab metrics ran without read-only PGOPTIONS: %q", b)
	}
}

// Review F5: a failed emit is retried on the next run; the raised state persists.
func TestHealthRetriesFailedEmit(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	os.WriteFile(filepath.Join(h.dir, "emit-fails"), nil, 0o644)
	h.set(t, false, false)
	h.run(t)
	if _, _, code := h.run(t); code == 0 {
		t.Fatalf("a failed emit must make the run fail")
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "logger.log")); !strings.Contains(string(b), "user.crit") {
		t.Errorf("a failed emit must log at crit priority: %q", b)
	}
	os.Remove(filepath.Join(h.dir, "emit-fails"))
	if _, se, code := h.run(t); code != 0 {
		t.Fatalf("retry run: exit %d: %s", code, se)
	}
	if ev := h.events(t); len(ev) != 1 || ev[0]["event"] != "raise" {
		t.Fatalf("the raise was not retried once: %v", ev)
	}
	h.run(t)
	if ev := h.events(t); len(ev) != 1 {
		t.Fatalf("raised state lost between runs: %v", ev)
	}
}

// Review F6: a disabled unit or a maintenance window is not an outage.
func TestHealthSuppression(t *testing.T) {
	needBash(t)
	t.Run("disabled unit", func(t *testing.T) {
		h := newHealth(t)
		os.WriteFile(filepath.Join(h.dir, "enabled"), []byte("disabled\n"), 0o644)
		h.set(t, false, false)
		for i := 0; i < 3; i++ {
			if _, se, code := h.run(t); code != 0 {
				t.Fatalf("exit %d: %s", code, se)
			}
		}
		if ev := h.events(t); len(ev) != 0 {
			t.Errorf("a disabled unit raised: %v", ev)
		}
	})
	t.Run("maintenance window", func(t *testing.T) {
		h := newHealth(t)
		if _, se, code := runBash(t, h.env, `exec "$1" --maintenance fabd 10`, abs(t, "fabd-health.sh")); code != 0 {
			t.Fatalf("maintenance: exit %d: %s", code, se)
		}
		h.set(t, false, false)
		h.run(t)
		h.run(t)
		if ev := h.events(t); len(ev) != 0 {
			t.Errorf("raised inside a maintenance window: %v", ev)
		}
		// expired window: checks resume
		os.WriteFile(filepath.Join(h.state, "fabd.maintenance"), []byte("1\n"), 0o600)
		h.run(t)
		h.run(t)
		if ev := h.events(t); len(ev) != 1 {
			t.Errorf("an expired maintenance window still suppressed: %v", ev)
		}
	})
	t.Run("maintenance bounds", func(t *testing.T) {
		h := newHealth(t)
		for _, m := range []string{"0", "241", "x"} {
			if _, _, code := runBash(t, h.env, `exec "$1" --maintenance fabd "$2"`, abs(t, "fabd-health.sh"), m); code == 0 {
				t.Errorf("--maintenance %s accepted", m)
			}
		}
	})
}

// Review F3: --clear clears a raised alert once, and is quiet otherwise.
func TestHealthClear(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	clear := func() int {
		_, _, code := runBash(t, h.env, `exec "$1" --clear fabd`, abs(t, "fabd-health.sh"))
		return code
	}
	if clear() != 0 || len(h.events(t)) != 0 {
		t.Fatalf("--clear with nothing raised emitted or failed: %v", h.events(t))
	}
	h.set(t, false, false)
	h.run(t)
	h.run(t)
	if clear() != 0 {
		t.Fatal("--clear failed")
	}
	ev := h.events(t)
	if len(ev) != 2 || ev[1]["event"] != "clear" {
		t.Fatalf("want raise then clear, got %v", ev)
	}
}

// Review F7: the check connects as fabd-health, never as a second "fabd".
func TestHealthApplicationName(t *testing.T) {
	needBash(t)
	h := newHealth(t)
	h.env = append(h.env, "FABRIC_DSN=postgres://u:SENTINEL-DSN@127.0.0.1:55990/fabric_live?application_name=fabd&sslmode=disable")
	h.set(t, true, true)
	so, se, code := h.run(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, "seen-dsn"))
	seen := string(b)
	if !strings.Contains(seen, "application_name=fabd-health") || strings.Contains(seen, "application_name=fabd&") || !strings.Contains(seen, "sslmode=disable") {
		t.Errorf("fab metrics got DSN query %q", seen[strings.Index(seen, "?"):])
	}
	if strings.Contains(so+se, "SENTINEL-DSN") {
		t.Errorf("DSN printed")
	}
}
