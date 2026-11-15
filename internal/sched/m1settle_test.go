package sched

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/records"
)

// m1Fixture is one bound attempt ready to settle: token A captured as v0
// and bound, then rotated to B (v1 current), so v0 is kept only while the
// attempt is unsettled. Its raw streams and run roots hold forms of A.
type m1Fixture struct {
	s                           *Scheduler
	pol                         credential.Policy
	bindings, sdir, run, recDir string
	tokA                        []byte
	mission                     domain.MissionID
	attempt                     domain.AttemptID
}

// inProcessSweep is the Launcher a test uses: the real sweep matcher, run in
// this process over the roots after "--".
func inProcessSweep(argv, env []string, stdin []byte) ([]byte, int, error) {
	i := 0
	for i < len(argv) && argv[i] != "--" {
		i++
	}
	rep := sweep.Run(stdin, argv[i+1:], sweep.DefaultLimits, time.Now)
	out, err := json.Marshal(rep)
	return out, 0, err
}

// sweepVia is a SweepFor factory that hands every settle launch.
func sweepVia(launch sweep.Launcher) func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
	return func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) { return launch, nil }
}

func newM1Fixture(t *testing.T, withSweep bool) *m1Fixture {
	return newM1FixtureFor(t, withSweep, "m_settle", "r_settle_1")
}

func newM1FixtureFor(t *testing.T, withSweep bool, m domain.MissionID, a domain.AttemptID) *m1Fixture {
	t.Helper()
	root := t.TempDir()
	f := &m1Fixture{mission: m, attempt: a}
	secret := filepath.Join(root, "secret")
	f.bindings = filepath.Join(root, "bindings")
	f.sdir = filepath.Join(root, "streams", string(f.attempt))
	f.run = filepath.Join(root, "run", string(f.attempt))
	f.recDir = filepath.Join(root, "records")
	for _, d := range []string{secret, f.bindings, f.sdir, f.recDir, filepath.Join(f.run, "claude-config"), filepath.Join(f.run, "home"), filepath.Join(f.run, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.pol = credential.Policy{Dir: secret, WorkerUID: 999999, WorkerGIDs: []int{999998}, OwnerUID: -1, GroupGID: -1}
	f.tokA = synthTok(108)
	if _, err := f.pol.Capture(f.tokA); err != nil {
		t.Fatal(err)
	}
	idKey := bytes.Repeat([]byte{7}, 32)
	others := func() []credential.OtherCredential {
		return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: []byte("synthetic-goodqa-key-0123456789abcdef")},
			{Kind: credential.KindDSN, Name: "runner", Value: []byte(numericPW)}}
	}
	if _, err := f.pol.PrepareLaunch(f.bindings, idKey, string(f.mission), string(f.attempt), others()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pol.Rotate(synthTok(108), f.bindings); err != nil {
		t.Fatal(err)
	}
	if !f.versionKept(t) {
		t.Fatal("bound v0 removed before settle: the fixture is vacuous")
	}
	text, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "env " + string(f.tokA)}}}})
	stream := initEv + "\n" + string(text) + "\n" + okResult + "\n"
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.sdir, "stream.jsonl"), stream)
	write(filepath.Join(f.sdir, "stderr.log"), "warn "+base64.StdEncoding.EncodeToString(f.tokA)+"\n")
	write(filepath.Join(f.run, "home", ".claude.json"), `{"oauth":"`+string(f.tokA)+`"}`)
	cfg := Config{RecordsDir: f.recDir, FabBin: "/usr/local/bin/fab",
		Credential: &CredentialLaunch{Policy: f.pol, BindingsDir: f.bindings, IDKey: idKey, Others: others}}
	if withSweep {
		cfg.SweepFor = sweepVia(inProcessSweep)
	}
	f.s = &Scheduler{cfg: cfg}
	return f
}

func (f *m1Fixture) versionKept(t *testing.T) bool {
	_, err := os.Lstat(filepath.Join(f.pol.Dir, credential.VersionName(0)))
	return err == nil
}

func (f *m1Fixture) settled() bool {
	_, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".settled"))
	return err == nil
}

func (f *m1Fixture) rawKept() bool {
	_, err := os.Lstat(filepath.Join(f.sdir, "stream.jsonl"))
	return err == nil
}

func (f *m1Fixture) kinds(t *testing.T) map[records.Kind]string {
	rs, _ := records.Read(f.recDir, f.attempt)
	out := map[records.Kind]string{}
	for _, r := range rs {
		out[r.Kind] = r.Class
	}
	return out
}

func (f *m1Fixture) settle(rc *host.StopReceipt) (m1Settled, string) {
	var redacted string
	out := f.s.settleM1(f.mission, f.attempt, 1, f.sdir, f.run, 0, rc, func(cmd *domain.Command, red string, b *credential.Bound) {
		data, _ := os.ReadFile(filepath.Join(red, "stream.jsonl"))
		redacted = string(data)
	})
	return out, redacted
}

var verified = &host.StopReceipt{Verified: true}

// The clean path: the effect step sees only the redacted copy; the sweep
// counts the planted token; the raw streams are removed; the attempt is
// marked settled and GC drops the bound token version.
func TestM1SettleCleanPath(t *testing.T) {
	f := newM1Fixture(t, true)
	out, redacted := f.settle(verified)
	if out.Cmd == nil || out.Cmd.Outcome != "checkpoint" || out.Blocked != "" {
		t.Fatalf("settle: %+v", out)
	}
	if redacted == "" || strings.Contains(redacted, string(f.tokA[12:40])) {
		t.Fatal("the effect step did not get a redacted copy free of the token")
	}
	if !out.Exposed || !strings.HasPrefix(out.Cmd.Reason, "m1:credential_exposure") || !out.Sweep.Complete || !out.Sweep.Exposed() {
		t.Fatalf("exposure not recorded: exposed=%v reason=%q sweep=%+v", out.Exposed, out.Cmd.Reason, out.Sweep)
	}
	k := f.kinds(t)
	if _, ok := k[records.CredentialExposure]; !ok {
		t.Fatalf("records %v", k)
	}
	if _, ok := k[records.SweepIncomplete]; ok {
		t.Fatal("a complete sweep recorded sweep_incomplete")
	}
	if f.rawKept() || !f.settled() || f.versionKept(t) {
		t.Fatalf("raw kept=%v settled=%v v0 kept=%v", f.rawKept(), f.settled(), f.versionKept(t))
	}
	if _, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".material")); err == nil {
		t.Fatal("material kept after a complete sweep")
	}
	if data, err := os.ReadFile(filepath.Join(f.sdir, sweepRecordName)); err != nil || bytes.Contains(data, f.tokA[12:40]) {
		t.Fatalf("sweep record: %v", err)
	}
}

// Owner ruling D (AC-18 verify 6): an incomplete sweep keeps only the
// material; the token version file is still removed.
func TestM1SettleIncompleteKeepsOnlyMaterial(t *testing.T) {
	for name, tc := range map[string]struct {
		sweep bool
		rc    *host.StopReceipt
	}{"no launcher": {false, verified}, "unverified stop": {true, &host.StopReceipt{}}, "no receipt": {true, nil}} {
		t.Run(name, func(t *testing.T) {
			f := newM1Fixture(t, tc.sweep)
			out, _ := f.settle(tc.rc)
			if out.Sweep.Complete {
				t.Fatal("sweep complete")
			}
			if _, ok := f.kinds(t)[records.SweepIncomplete]; !ok {
				t.Fatal("no sweep_incomplete record")
			}
			if _, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".material")); err != nil {
				t.Fatal("no material kept")
			}
			if !f.settled() || f.versionKept(t) {
				t.Fatalf("settled=%v v0 kept=%v", f.settled(), f.versionKept(t))
			}
			m, _ := os.ReadFile(filepath.Join(f.bindings, string(f.attempt)+".material"))
			if bytes.Contains(m, f.tokA[12:40]) || bytes.Contains(m, []byte(base64.StdEncoding.EncodeToString(f.tokA)[16:40])) {
				t.Fatal("material holds token bytes")
			}
		})
	}
}

// Owner ruling C: a fail-closed settle finishes abandoned with
// m1:delivery_blocked:{class}, holds the mission for the four classes, runs
// no sweep without bound values, and keeps the raw files.
func TestM1SettleFailClosed(t *testing.T) {
	t.Run("binding deleted", func(t *testing.T) {
		f := newM1Fixture(t, true)
		os.Remove(filepath.Join(f.bindings, string(f.attempt)+".json"))
		out, redacted := f.settle(verified)
		if out.Blocked != credential.ClassMaterialUnavailable || !out.Hold || out.Cmd == nil || out.Cmd.Outcome != "abandoned" ||
			out.Cmd.Reason != "m1:delivery_blocked:"+credential.ClassMaterialUnavailable || redacted != "" {
			t.Fatalf("settle: %+v", out)
		}
		if !f.rawKept() || !f.settled() {
			t.Fatal("raw files removed or not marked settled")
		}
		if out.Sweep.Complete || len(out.Sweep.Counts) != 0 {
			t.Fatal("a sweep ran without bound values")
		}
		if f.kinds(t)[records.DeliveryBlocked] != credential.ClassMaterialUnavailable {
			t.Fatal("no delivery_blocked record")
		}
	})
	t.Run("goodqa key changed", func(t *testing.T) {
		f := newM1Fixture(t, true)
		f.s.cfg.Credential.Others = func() []credential.OtherCredential {
			return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: []byte("synthetic-goodqa-key-CHANGED-0123456")},
				{Kind: credential.KindDSN, Name: "runner", Value: []byte(numericPW)}}
		}
		out, _ := f.settle(verified)
		if out.Blocked != credential.ClassRotatedMidAttempt || !out.Hold {
			t.Fatalf("settle: %+v", out)
		}
	})
	t.Run("numeric hit", func(t *testing.T) {
		f := newM1Fixture(t, true)
		// The runner password inside a JSON number: no length-preserving
		// rewrite keeps the number valid, so redaction blocks.
		num := `{"type":"assistant","n":` + numericPW + `}`
		os.WriteFile(filepath.Join(f.sdir, "stream.jsonl"), []byte(initEv+"\n"+num+"\n"+okResult+"\n"), 0o600) //nolint:errcheck
		out, redacted := f.settle(verified)
		if out.Blocked != credential.ClassNumericHit || !out.Hold || redacted != "" || !f.rawKept() {
			t.Fatalf("settle: blocked=%q hold=%v raw kept=%v", out.Blocked, out.Hold, f.rawKept())
		}
		if out.Cmd == nil || !strings.Contains(out.Cmd.Reason, "m1:delivery_blocked:"+credential.ClassNumericHit) {
			t.Fatalf("finish: %+v", out.Cmd)
		}
		if !f.settled() || f.versionKept(t) {
			t.Fatal("not settled")
		}
	})
}

// numericPW is a synthetic all-digit DSN password, so a fixture can place
// it inside a JSON number.
const numericPW = "86753091234567890123"

// Every step of the sequence, failing (and a panic): MarkSettled still runs
// (unless the mark itself is the injected failure), and no raw token
// version remains after settle (unless the mark or the GC is the failure,
// where keeping it is the safe outcome).
func TestM1SettleEveryExitMarksSettled(t *testing.T) {
	steps := []string{stepLoad, stepRedact, stepSweep, stepSweepRec, stepRecords, stepMaterial, stepRawRemove, stepMark, stepGC, "panic"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			// The material step runs only on an incomplete sweep.
			f := newM1Fixture(t, step != stepMaterial)
			injected := false
			f.s.settleFault = func(s string) error {
				if step == "panic" && s == stepSweep {
					panic("injected")
				}
				if s == step {
					injected = true
					return errors.New("injected " + s)
				}
				return nil
			}
			out, _ := f.settle(verified)
			if step != "panic" && !injected {
				t.Fatalf("step %s never ran: the case is vacuous", step)
			}
			switch step {
			case stepMark:
				if f.settled() || !f.versionKept(t) {
					t.Fatal("a failed mark must leave the version for GC to keep")
				}
				return
			case stepGC:
				if !f.settled() {
					t.Fatal("not marked settled")
				}
				return
			}
			if !f.settled() {
				t.Fatalf("%s: not marked settled", step)
			}
			if f.versionKept(t) {
				t.Fatalf("%s: raw token version file remains after settle", step)
			}
			if step == "panic" && (out.Blocked != classSettlePanic || out.Hold || out.Cmd != nil) {
				t.Fatalf("panic: %+v", out)
			}
			if step == stepSweep {
				if _, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".material")); err != nil {
					t.Fatal("incomplete sweep kept no material")
				}
			}
		})
	}
}

// A crash after the redacted copy and the raw removal, before MarkSettled:
// the rerun reuses the finished redaction instead of redacting an empty
// stream as clean.
func TestM1SettleReentryReusesRedaction(t *testing.T) {
	f := newM1Fixture(t, true)
	f.s.settleFault = func(s string) error {
		if s == stepMark || s == stepGC {
			return errors.New("crash")
		}
		return nil
	}
	first, _ := f.settle(verified)
	if f.rawKept() || f.settled() {
		t.Fatal("fixture: raw should be gone and the mark missing")
	}
	f.s.settleFault = nil
	again, redacted := f.settle(verified)
	if again.Cmd == nil || again.Cmd.Outcome != first.Cmd.Outcome || !again.Exposed || redacted == "" {
		t.Fatalf("rerun: %+v", again)
	}
	if !f.settled() || f.versionKept(t) {
		t.Fatal("rerun did not settle")
	}
}

// Spec v15 item 1 (NEG-35 precondition): each load-failure class reads
// nothing (no redacted copy), runs no sweep (no sweep record, the launcher
// never called), keeps every raw file, and records sweep_incomplete with
// class material_unavailable beside its delivery_blocked record.
func TestM1SettleLoadFailureRecordsUnswept(t *testing.T) {
	for class, breakIt := range map[string]func(f *m1Fixture){
		credential.ClassMaterialUnavailable: func(f *m1Fixture) { os.Remove(filepath.Join(f.bindings, string(f.attempt)+".json")) },
		credential.ClassRotatedMidAttempt: func(f *m1Fixture) {
			f.s.cfg.Credential.Others = func() []credential.OtherCredential {
				return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: []byte("synthetic-goodqa-key-CHANGED-0123456")},
					{Kind: credential.KindDSN, Name: "runner", Value: []byte(numericPW)}}
			}
		},
	} {
		t.Run(class, func(t *testing.T) {
			f := newM1Fixture(t, true)
			launched := 0
			built := 0
			f.s.cfg.SweepFor = func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
				built++
				return func(argv, env []string, stdin []byte) ([]byte, int, error) {
					launched++
					return inProcessSweep(argv, env, stdin)
				}, nil
			}
			breakIt(f)
			out, redacted := f.settle(verified)
			if out.Blocked != class || !out.Hold || redacted != "" {
				t.Fatalf("settle: %+v", out)
			}
			if launched != 0 || built != 0 {
				t.Fatalf("a sweep launcher was built (%d) or ran (%d) without bound values", built, launched)
			}
			for _, p := range []string{filepath.Join(f.sdir, redactedDirName), filepath.Join(f.sdir, sweepRecordName)} {
				if _, err := os.Lstat(p); err == nil {
					t.Fatalf("%s exists: the stream was read or swept", filepath.Base(p))
				}
			}
			for _, p := range []string{filepath.Join(f.sdir, "stream.jsonl"), filepath.Join(f.sdir, "stderr.log"), filepath.Join(f.run, "home", ".claude.json")} {
				if _, err := os.Lstat(p); err != nil {
					t.Fatalf("%s removed", filepath.Base(p))
				}
			}
			k := f.kinds(t)
			if k[records.DeliveryBlocked] != class {
				t.Fatalf("delivery_blocked %q", k[records.DeliveryBlocked])
			}
			if c, ok := k[records.SweepIncomplete]; !ok || c != sweepMaterialUnavailable {
				t.Fatalf("sweep_incomplete record %q present=%v", c, ok)
			}
			if !f.settled() {
				t.Fatal("not marked settled")
			}
		})
	}
}

// AC-35(h), spec v16: with no sweep launcher configured the settle is
// sweep_incomplete, never clean: the report is incomplete with the
// no-launcher reason, the record is written, and only the sweep material
// is kept for a later re-sweep.
func TestM1SettleNoLauncherIsIncomplete(t *testing.T) {
	f := newM1Fixture(t, false)
	if f.s.cfg.SweepFor != nil {
		t.Fatal("fixture: a launcher is configured")
	}
	out, _ := f.settle(verified)
	if out.Sweep.Complete {
		t.Fatal("no launcher reported a complete sweep")
	}
	found := false
	for _, r := range out.Sweep.Reasons {
		found = found || r == reasonNoLauncher
	}
	if !found {
		t.Fatalf("sweep reasons %v lack %s", out.Sweep.Reasons, reasonNoLauncher)
	}
	if _, ok := f.kinds(t)[records.SweepIncomplete]; !ok {
		t.Fatal("no sweep_incomplete record")
	}
	if _, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".material")); err != nil {
		t.Fatal("no sweep material kept")
	}
	if !f.settled() || f.versionKept(t) {
		t.Fatalf("settled=%v version kept=%v", f.settled(), f.versionKept(t))
	}
}

// AC-35(h): a SweepFor error is sweep_incomplete with the no-launcher
// reason, never clean, and the sweep material is kept.
func TestM1SettleSweepFactoryErrorIsIncomplete(t *testing.T) {
	f := newM1Fixture(t, false)
	f.s.cfg.SweepFor = func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
		return nil, errors.New("no run record")
	}
	out, _ := f.settle(verified)
	if out.Sweep.Complete || len(out.Sweep.Reasons) != 1 || out.Sweep.Reasons[0] != reasonNoLauncher {
		t.Fatalf("factory error gave sweep %+v; want incomplete, %s", out.Sweep, reasonNoLauncher)
	}
	if _, ok := f.kinds(t)[records.SweepIncomplete]; !ok {
		t.Fatal("no sweep_incomplete record")
	}
	if _, err := os.Lstat(filepath.Join(f.bindings, string(f.attempt)+".material")); err != nil {
		t.Fatal("no sweep material kept")
	}
	if !f.settled() || f.versionKept(t) {
		t.Fatalf("settled=%v version kept=%v", f.settled(), f.versionKept(t))
	}
}

// AC-16, AC-35(h): the worker-manager sweep launcher is built and run only
// after a Verified stop with no survivors and no escapees, for this
// attempt's run directory, with the fixed argv and environment. Any other
// stop launches nothing and reads stop_unverified.
func TestM1SettleSweepOnlyAfterVerifiedStop(t *testing.T) {
	for name, rc := range map[string]*host.StopReceipt{
		"no receipt": nil,
		"unverified": {Verified: false},
		"survivor":   {Verified: true, Survivors: []host.Process{{PID: 7}}},
		"escaped":    {Verified: true, Escaped: []host.Process{{PID: 8, Escaped: true}}},
		"verified":   verified,
	} {
		t.Run(name, func(t *testing.T) {
			f := newM1Fixture(t, false)
			type built struct {
				m      domain.MissionID
				a      domain.AttemptID
				runDir string
			}
			var got []built
			var argv, env []string
			f.s.cfg.SweepFor = func(m domain.MissionID, a domain.AttemptID, runDir string) (sweep.Launcher, error) {
				got = append(got, built{m, a, runDir})
				return func(av, ev []string, stdin []byte) ([]byte, int, error) {
					argv, env = av, ev
					return inProcessSweep(av, ev, stdin)
				}, nil
			}
			out, _ := f.settle(rc)
			if name != "verified" {
				if len(got) != 0 || argv != nil {
					t.Fatalf("launcher built %d times, ran=%v on a %s stop", len(got), argv != nil, name)
				}
				if out.Sweep.Complete || len(out.Sweep.Reasons) != 1 || out.Sweep.Reasons[0] != sweep.ReasonStopUnverified {
					t.Fatalf("sweep %+v; want incomplete, %s", out.Sweep, sweep.ReasonStopUnverified)
				}
				return
			}
			if len(got) != 1 || got[0] != (built{f.mission, f.attempt, f.run}) {
				t.Fatalf("factory calls %+v; want one for %s/%s/%s", got, f.mission, f.attempt, f.run)
			}
			wantArgv := append([]string{"/usr/local/bin/fab", "__sweep", "--"}, sweepRoots(f.run)...)
			if strings.Join(argv, " ") != strings.Join(wantArgv, " ") || strings.Join(env, " ") != strings.Join(sweep.Env, " ") {
				t.Fatalf("argv %v env %v", argv, env)
			}
			if !out.Sweep.Complete {
				t.Fatalf("verified sweep incomplete: %+v", out.Sweep)
			}
		})
	}
}
