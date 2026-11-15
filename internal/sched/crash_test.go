package sched

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"fabric/internal/crashpoint"
	"fabric/internal/credential"
	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

// goodqa#15966 crash-point brief section 3: TestCrashPoints kills a real
// child process (a re-run of this test binary) at every occurrence of every
// crash point a scenario reaches, before and after the write, then restarts
// a fresh Scheduler over the same database and directories and checks the
// invariants. The model is a process crash: an uncommitted transaction dies
// with the child's connection, and written files survive. Power-loss
// (fsync) durability is the existing seams' to prove, not this harness's.
//
// The parent arranges the state, the child acts, the parent forces the
// lease past expiry (the fake clock) and runs two fresh schedulers' ticks
// with the runner read back as gone, then asserts:
//   - I1: a mission that owes a hold is held once recovery completes;
//   - I2: no START of a mission that owes a hold (nothing releases it here);
//   - I4: nothing is marked settled while a hold it owes is uncommitted,
//     checked right after the crash and after recovery;
//   - I5: at most 1 comment transmitted per mission (a durable fake
//     EffectStep counts transmissions; the effects package's own idempotence
//     is out of scope);
//   - I6: after recovery no token version file survives a completed settle
//     (only .material may remain), and no binding is left unsettled except
//     by an unverified restart that holds the mission for the operator.
// I3 (release) is TestReleaseCrashPoints in cmd/fab.

const (
	crashEnvScenario = "FABRIC_CRASH_SCENARIO"
	crashEnvRoot     = "FABRIC_CRASH_ROOT"
	crashEnvRunner   = "FABRIC_CRASH_RUNNER_DSN"
	crashEnvStrict   = "FABRIC_CRASH_STRICT" // 1: known failures fail (failing-first evidence)
	crashEnvReport   = "FABRIC_CRASH_REPORT" // the per-case table is written here
	crashEnvOnly     = "FABRIC_CRASH_ONLY"   // optional scenario filter (comma-separated)
	crashMission     = domain.MissionID("m_crash")
)

// crashScenario is one path to crash. owes is the hold class the scenario
// owes ("" for none); unverified marks the scenario whose binding stays
// unsettled by design.
type crashScenario struct {
	name       string
	owes       string
	unverified bool
	arrange    func(t *testing.T, w *crashWorld)
	act        func(ctx context.Context, w *crashWorld) error
}

// crashWorld is the shared state of one case. Everything a child needs is
// derived from root, so parent and child build the same Config.
type crashWorld struct {
	root string
	st   *store.Store
	cfg  Config
	ln   *fakeLauncher
}

type crashState struct {
	Attempt domain.AttemptID `json:"attempt"`
	Fence   int64            `json:"fence"`
}

func (w *crashWorld) p(parts ...string) string {
	return filepath.Join(append([]string{w.root}, parts...)...)
}

func (w *crashWorld) bindings() string { return w.p("run-area", "bindings") }

func (w *crashWorld) mode(name, def string) string {
	b, err := os.ReadFile(w.p("mode-" + name))
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(b))
}

func (w *crashWorld) setMode(t *testing.T, name, v string) {
	t.Helper()
	if err := os.WriteFile(w.p("mode-"+name), []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *crashWorld) state() crashState {
	var s crashState
	b, _ := os.ReadFile(w.p("state.json"))
	json.Unmarshal(b, &s) //nolint:errcheck
	return s
}

func (w *crashWorld) saveState(t *testing.T, s crashState) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(w.p("state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *crashWorld) pol() credential.Policy {
	return credential.Policy{Dir: w.p("secret"), WorkerUID: 999999, WorkerGIDs: []int{999998}, OwnerUID: -1, GroupGID: -1}
}

func (w *crashWorld) others() []credential.OtherCredential {
	key := []byte(w.mode("goodqa", "synthetic-goodqa-key-0123456789abcdef"))
	return []credential.OtherCredential{{Kind: credential.KindGoodQA, Name: "goodqa", Value: key},
		{Kind: credential.KindDSN, Name: "runner", Value: []byte(numericPW)}}
}

// newCrashWorld builds the Config both processes use. The parent's
// launcher refuses every start, so a restart that dispatches never leaves a
// waiting goroutine behind (the START is still counted in the store).
func newCrashWorld(root string, st *store.Store, runnerDSN string, child bool) *crashWorld {
	w := &crashWorld{root: root, st: st}
	pin, _ := credential.FileSHA256(w.p("claude"))
	if w.mode("pin", "ok") == "bad" {
		pin = strings.Repeat("0", 64)
	}
	w.ln = &fakeLauncher{st: st, block: make(chan struct{})}
	if !child {
		w.ln.startErr = errors.New("crash harness: the restarted fabd launches nothing")
	}
	cfg := Config{Host: "h_t2", Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "test", Profile: "P1", MaxTurns: 5,
		MaxBudgetUSD: 0.01, Wall: time.Minute, WorkRoot: w.p("work"), RunnerDSN: runnerDSN,
		StreamRoot: w.p("run-area", "streams"), RecordsDir: w.p("run-area", "records"), FabBin: "/usr/local/bin/fab",
		Credential: &CredentialLaunch{Policy: w.pol(), BindingsDir: w.bindings(), IDKey: bytes.Repeat([]byte{7}, 32), PinnedSHA: pin, Others: w.others}}
	cfg.Launch.Binary = w.p("claude")
	if w.mode("preexec", "ok") == "refuse" {
		cfg.Launch.PreExec = func(string) error { return errors.New("crash harness: refused before exec") }
	}
	cfg.SweepFor = func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
		if w.mode("sweep", "ok") == "fail" {
			return nil, errors.New("crash harness: no sweep launcher")
		}
		return inProcessSweep, nil
	}
	cfg.VerifyGone = func(run domain.AttemptID, _ time.Time) (host.StopReceipt, error) {
		if w.mode("verify", "yes") != "yes" {
			return host.StopReceipt{Run: run}, errors.New("crash harness: not verified gone")
		}
		return host.StopReceipt{Run: run, Verified: true}, nil
	}
	cfg.EffectStep = func(_ context.Context, a EffectAttempt, _ func(context.Context) error) string {
		f, err := os.OpenFile(w.p("effects.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return ""
		}
		f.WriteString(string(a.Mission) + "\n") //nolint:errcheck
		f.Sync()                                //nolint:errcheck
		f.Close()
		return "m1:comment_posted"
	}
	w.cfg = cfg
	return w
}

// base lays out the directories, the pinned fake claude and token A as v0.
func (w *crashWorld) base(t *testing.T) []byte {
	t.Helper()
	for _, d := range []string{"secret", "work", "run-area", "run-area/bindings", "run-area/streams", "run-area/records"} {
		if err := os.MkdirAll(w.p(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(w.p("claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tok := synthTok(108)
	if _, err := w.pol().Capture(tok); err != nil {
		t.Fatal(err)
	}
	setCap(t, w.st, 5)
	admitSpec(t, w.st, crashMission, domain.ProviderClaudeCode, 1)
	return tok
}

// bound is a token-path attempt that ran: START and ACK committed, token A
// bound as v0 and then rotated to v1, and the raw streams and run roots
// holding forms of A.
func (w *crashWorld) bound(t *testing.T, stream func(tok []byte) string) {
	t.Helper()
	ctx := context.Background()
	tok := w.base(t)
	a := domain.AttemptID("r_crash_1")
	resp, err := w.st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: crashMission, Attempt: a,
		Host: w.cfg.Host, Role: "writer", Path: domain.ProviderClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := w.st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: crashMission, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	pol := w.pol()
	if _, err := pol.PrepareLaunch(w.bindings(), w.cfg.Credential.IDKey, string(crashMission), string(a), w.others()); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.Rotate(synthTok(108), w.bindings()); err != nil {
		t.Fatal(err)
	}
	w.saveState(t, crashState{Attempt: a, Fence: fence})
	sdir := filepath.Join(w.cfg.StreamRoot, string(a))
	run := filepath.Join(w.cfg.WorkRoot, "run", string(a))
	for _, d := range []string{sdir, filepath.Join(run, "claude-config"), filepath.Join(run, "home"), filepath.Join(run, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		filepath.Join(sdir, "stream.jsonl"):        stream(tok),
		filepath.Join(sdir, "stderr.log"):          "warn " + base64.StdEncoding.EncodeToString(tok) + "\n",
		filepath.Join(run, "home", ".claude.json"): `{"oauth":"` + string(tok) + `"}`,
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func okStream(tok []byte) string {
	text, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "env " + string(tok)}}}})
	return initEv + "\n" + string(text) + "\n" + okResult + "\n"
}

func refusedStream([]byte) string {
	return initEv + "\n" + `{"type":"assistant","session_id":"s1","error":"authentication_failed","message":{"usage":{}}}` + "\n" +
		`{"type":"result","subtype":"success","session_id":"s1","uuid":"r1","num_turns":1,"is_error":true,"result":"token expired"}` + "\n"
}

func numericStream([]byte) string {
	return initEv + "\n" + `{"type":"assistant","n":` + numericPW + `}` + "\n" + okResult + "\n"
}

// lapse forces every live lease of the mission past expiry: the fake clock
// of a restart long after the crash.
func (w *crashWorld) lapse(t *testing.T) {
	t.Helper()
	if _, err := w.st.Pool().Exec(context.Background(), `UPDATE attempts SET lease_until = now() - interval '1 second'
		WHERE mission_id = $1 AND lease_until IS NOT NULL`, string(crashMission)); err != nil {
		t.Fatal(err)
	}
}

func (w *crashWorld) ticks(ctx context.Context, n int) error {
	s := New(w.st, w.cfg, w.ln)
	s.prober = &fakeProber{alive: false, known: true}
	for i := 0; i < n; i++ {
		if _, err := s.Tick(ctx); err != nil {
			return err
		}
	}
	return nil
}

// live settles the bound attempt as a live exit after a verified Stop.
func (w *crashWorld) live(ctx context.Context) error {
	s := New(w.st, w.cfg, w.ln)
	st := w.state()
	run := filepath.Join(w.cfg.WorkRoot, "run", string(st.Attempt))
	return s.afterWaitRun(ctx, crashMission, st.Attempt, st.Fence, s.streamDir(run, st.Attempt), run, 0, nil, verified)
}

func crashScenarios() []crashScenario {
	liveAct := func(ctx context.Context, w *crashWorld) error { return w.live(ctx) }
	restartAct := func(ctx context.Context, w *crashWorld) error { return w.ticks(ctx, 3) }
	boundWith := func(stream func([]byte) string, also func(t *testing.T, w *crashWorld)) func(t *testing.T, w *crashWorld) {
		return func(t *testing.T, w *crashWorld) {
			w.bound(t, stream)
			if also != nil {
				also(t, w)
			}
		}
	}
	rotateOthers := func(t *testing.T, w *crashWorld) { w.setMode(t, "goodqa", "synthetic-goodqa-key-CHANGED-0123456") }
	lapsed := func(also func(t *testing.T, w *crashWorld)) func(t *testing.T, w *crashWorld) {
		return func(t *testing.T, w *crashWorld) {
			if also != nil {
				also(t, w)
			}
			w.lapse(t)
		}
	}
	return []crashScenario{
		{name: "clean-with-comment", arrange: boundWith(okStream, nil), act: liveAct},
		{name: "material-unavailable", owes: credential.ClassMaterialUnavailable, arrange: boundWith(okStream, func(t *testing.T, w *crashWorld) {
			os.Remove(filepath.Join(w.bindings(), string(w.state().Attempt)+".json")) //nolint:errcheck
		}), act: liveAct},
		{name: "rotated-mid-attempt", owes: credential.ClassRotatedMidAttempt, arrange: boundWith(okStream, rotateOthers), act: liveAct},
		{name: "numeric-hit", owes: credential.ClassNumericHit, arrange: boundWith(numericStream, nil), act: liveAct},
		{name: "broke-stream-planted", owes: credential.ClassBrokeStream, arrange: boundWith(okStream, func(t *testing.T, w *crashWorld) {
			// PLANTED: redaction_broke_stream is unreachable through
			// RedactStreams (a covered string byte is rewritten to 'X' per
			// escape unit, which never breaks a valid JSON line; an invalid
			// line is redacted as opaque text). A prior redaction's summary
			// carrying the class is planted so its settle and hold path is
			// still crashed.
			sdir := filepath.Join(w.cfg.StreamRoot, string(w.state().Attempt))
			os.MkdirAll(filepath.Join(sdir, redactedDirName), 0o700) //nolint:errcheck
			b, _ := json.Marshal(redactSummary{Blocked: credential.ClassBrokeStream})
			if err := os.WriteFile(filepath.Join(sdir, redactedDone), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}), act: liveAct},
		{name: "credential-rejected", owes: "credential_rejected", arrange: boundWith(refusedStream, nil), act: liveAct},
		{name: "sweep-incomplete", arrange: boundWith(okStream, func(t *testing.T, w *crashWorld) { w.setMode(t, "sweep", "fail") }), act: liveAct},
		{name: "recovery-verified-clean", arrange: boundWith(okStream, lapsed(nil)), act: restartAct},
		{name: "recovery-verified-hold", owes: credential.ClassRotatedMidAttempt, arrange: boundWith(okStream, lapsed(rotateOthers)), act: restartAct},
		{name: "recovery-unverified", owes: classRestartUnverified, unverified: true,
			arrange: boundWith(okStream, lapsed(func(t *testing.T, w *crashWorld) { w.setMode(t, "verify", "no") })), act: restartAct},
		{name: "launch", arrange: func(t *testing.T, w *crashWorld) { w.base(t) }, act: func(ctx context.Context, w *crashWorld) error {
			return New(w.st, w.cfg, w.ln).runOnce(ctx, crashMission)
		}},
		// A refusal after ACK and before exec: abandoned in prelaunch, then
		// the unstarted settle.
		{name: "unstarted", arrange: func(t *testing.T, w *crashWorld) { w.base(t); w.setMode(t, "preexec", "refuse") }, act: func(ctx context.Context, w *crashWorld) error {
			return New(w.st, w.cfg, w.ln).runOnce(ctx, crashMission)
		}},
		// A refusal after the binding and before ACK (the pinned binary
		// does not match): abandoned in prelaunch.
		{name: "refused-after-binding", arrange: func(t *testing.T, w *crashWorld) { w.base(t); w.setMode(t, "pin", "bad") }, act: func(ctx context.Context, w *crashWorld) error {
			return New(w.st, w.cfg, w.ln).runOnce(ctx, crashMission)
		}},
	}
}

// TestCrashChild is the child half: it acts out one scenario against the
// parent's database and directories. It is skipped in a normal run.
func TestCrashChild(t *testing.T) {
	name := os.Getenv(crashEnvScenario)
	if name == "" {
		t.Skip("run by TestCrashPoints only")
	}
	st, err := store.Open(context.Background(), storetest.DSN) // never Open(t): that empties the database
	if err != nil {
		t.Fatal("open the parent's database failed")
	}
	defer st.Close()
	st.Lease = 30 * time.Second
	w := newCrashWorld(os.Getenv(crashEnvRoot), st, os.Getenv(crashEnvRunner), true)
	for _, sc := range crashScenarios() {
		if sc.name == name {
			if err := sc.act(context.Background(), w); err != nil {
				fmt.Fprintf(os.Stderr, "crash child %s: act: %v\n", name, err)
			}
			return
		}
	}
	t.Fatalf("no scenario %s", name)
}

type crashRun struct {
	exit   int
	stderr string
}

func runCrashChild(t *testing.T, w *crashWorld, sc string, arm string) crashRun {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), storetest.ChildDSNEnv+"="+storetest.DSN, crashEnvScenario+"="+sc, crashEnvRoot+"="+w.root,
		crashEnvRunner+"="+w.cfg.RunnerDSN, crashpoint.Env+"="+arm)
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	r := crashRun{stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
	default:
		t.Fatalf("child did not run: %v", err)
	}
	return r
}

// traced is one reached point occurrence, "name:pos:n".
func traced(stderr string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(stderr))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), crashpoint.TraceMarker); ok {
			out = append(out, v)
		}
	}
	return out
}

// crashKnown are invariant failures with a fix commit pending; the case
// logs KNOWN instead of failing. FABRIC_CRASH_STRICT=1 fails them too (the
// failing-first evidence), and an entry that no longer fails is an error.
var crashKnown = map[string]string{
	"broke-stream-planted/hold.operator:before:1":    "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"broke-stream-planted/settle.gc:after:1":         "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"broke-stream-planted/settle.gc:before:1":        "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"broke-stream-planted/settle.mark:after:1":       "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"credential-rejected/hold.operator:before:1":     "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"credential-rejected/settle.gc:after:1":          "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"credential-rejected/settle.gc:before:1":         "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"credential-rejected/settle.mark:after:1":        "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"material-unavailable/hold.operator:before:1":    "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"material-unavailable/settle.gc:after:1":         "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"material-unavailable/settle.gc:before:1":        "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"material-unavailable/settle.mark:after:1":       "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"material-unavailable/settle.mark:before:1":      "F3: a lost binding record is never held (fix: M1 flag on START, generalised scan)",
	"material-unavailable/settle.record:after:1":     "F3: a lost binding record is never held (fix: M1 flag on START, generalised scan)",
	"material-unavailable/settle.record:after:2":     "F3: a lost binding record is never held (fix: M1 flag on START, generalised scan)",
	"material-unavailable/settle.record:before:1":    "F3: a lost binding record is never held (fix: M1 flag on START, generalised scan)",
	"material-unavailable/settle.record:before:2":    "F3: a lost binding record is never held (fix: M1 flag on START, generalised scan)",
	"numeric-hit/hold.operator:before:1":             "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"numeric-hit/settle.gc:after:1":                  "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"numeric-hit/settle.gc:before:1":                 "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"numeric-hit/settle.mark:after:1":                "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"refused-after-binding/finish.prelaunch:after:1": "F2: an unsettled binding no scan visits (fix: unstarted settle first, generalised scan)",
	"refused-after-binding/launch.start:before:1":    "F2: an unsettled binding no scan visits (fix: unstarted settle first, generalised scan)",
	"rotated-mid-attempt/hold.operator:before:1":     "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"rotated-mid-attempt/settle.gc:after:1":          "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"rotated-mid-attempt/settle.gc:before:1":         "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"rotated-mid-attempt/settle.mark:after:1":        "F1: the live path marks settled before the hold (fix: beforeMark on the live path)",
	"unstarted/finish.prelaunch:after:1":             "F2: an unsettled binding no scan visits (fix: unstarted settle first, generalised scan)",
	"unstarted/settle.unstarted.mark:before:1":       "F2: an unsettled binding no scan visits (fix: unstarted settle first, generalised scan)",
}

func (w *crashWorld) attempts(t *testing.T) []domain.AttemptID {
	t.Helper()
	rows, err := w.st.Pool().Query(context.Background(), `SELECT id FROM attempts WHERE mission_id = $1 ORDER BY started_at`, string(crashMission))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []domain.AttemptID
	for rows.Next() {
		var a domain.AttemptID
		rows.Scan(&a) //nolint:errcheck
		out = append(out, a)
	}
	return out
}

// boundAttempts are the attempts with a binding record, and which are settled.
func (w *crashWorld) boundAttempts() map[string]bool {
	out := map[string]bool{}
	ents, _ := os.ReadDir(w.bindings())
	for _, e := range ents {
		if a, ok := strings.CutSuffix(e.Name(), ".json"); ok && e.Name() != "retained.json" {
			_, err := os.Lstat(filepath.Join(w.bindings(), a+".settled"))
			out[a] = err == nil
		}
	}
	return out
}

func (w *crashWorld) settledAny() bool {
	ents, _ := os.ReadDir(w.bindings())
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".settled") {
			return true
		}
	}
	return false
}

func (w *crashWorld) held(t *testing.T) bool {
	t.Helper()
	h, err := w.st.OperatorHeld(context.Background(), crashMission)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// violations checks the invariants; final is after recovery.
func (w *crashWorld) violations(t *testing.T, sc crashScenario, final bool, firstAttempts int) []string {
	t.Helper()
	var v []string
	held := w.held(t)
	if sc.owes != "" && w.settledAny() && !held {
		v = append(v, "I4: settled while the owed "+sc.owes+" hold is uncommitted")
	}
	if !final {
		return v
	}
	if sc.owes != "" && !held {
		v = append(v, "I1: owes "+sc.owes+", not held after recovery")
	}
	if n := len(w.attempts(t)); sc.owes != "" && n > firstAttempts {
		v = append(v, fmt.Sprintf("I2: %d START(s) of a mission that owes %s", n-firstAttempts, sc.owes))
	}
	if b, _ := os.ReadFile(w.p("effects.log")); strings.Count(string(b), string(crashMission)+"\n") > 1 {
		v = append(v, fmt.Sprintf("I5: %d comments transmitted", strings.Count(string(b), string(crashMission)+"\n")))
	}
	versions := 0
	ents, _ := os.ReadDir(w.p("secret"))
	cur, _ := os.Stat(w.p("secret", credential.CurrentName))
	for _, e := range ents {
		if fi, err := os.Stat(w.p("secret", e.Name())); err == nil && e.Name() != credential.CurrentName && strings.HasPrefix(e.Name(), "v") && !os.SameFile(fi, cur) {
			versions++
		}
	}
	unsettled := 0
	for a, settled := range w.boundAttempts() {
		if !settled {
			unsettled++
			if !sc.unverified {
				v = append(v, "I6: binding "+a+" left unsettled after recovery (its token version stays pinned)")
			}
		}
	}
	if unsettled == 0 && versions > 0 {
		v = append(v, fmt.Sprintf("I6: %d non-current token version file(s) survive a completed settle", versions))
	}
	return v
}

func TestCrashPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child per crash point")
	}
	top := storetest.Open(t)
	runner := storetest.RunnerDSN(t, top)
	strict := os.Getenv(crashEnvStrict) == "1"
	only := map[string]bool{}
	for _, n := range strings.Split(os.Getenv(crashEnvOnly), ",") {
		if n != "" {
			only[n] = true
		}
	}
	var report []string
	reached := map[string]bool{}
	knownSeen := map[string]bool{}
	fresh := func(t *testing.T, sc crashScenario) *crashWorld {
		st := storetest.Open(t)
		st.Lease = 30 * time.Second
		w := newCrashWorld(t.TempDir(), st, runner, false)
		sc.arrange(t, w)
		w = newCrashWorld(w.root, st, runner, false) // modes the arrange set
		return w
	}
	for _, sc := range crashScenarios() {
		if len(only) > 0 && !only[sc.name] {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			w := fresh(t, sc)
			tr := runCrashChild(t, w, sc.name, crashpoint.Trace)
			if tr.exit != 0 {
				t.Fatalf("trace run exit %d: %s", tr.exit, tr.stderr)
			}
			points := traced(tr.stderr)
			if len(points) == 0 {
				t.Fatalf("trace run reached no point: %s", tr.stderr)
			}
			for _, pt := range points {
				reached[strings.SplitN(pt, ":", 2)[0]] = true
			}
			t.Logf("%s: %d point occurrences", sc.name, len(points))
			for _, pt := range points {
				t.Run(pt, func(t *testing.T) {
					w := fresh(t, sc)
					firstAttempts := len(w.attempts(t))
					r := runCrashChild(t, w, sc.name, pt)
					if r.exit != crashpoint.ExitCode || !strings.Contains(r.stderr, crashpoint.Marker+pt+"\n") {
						t.Errorf("%s at %s: the child did not die there (exit %d)", sc.name, pt, r.exit)
						report = append(report, fmt.Sprintf("| %s | %s | not reached | - |", sc.name, pt))
						return
					}
					if sc.name == "launch" || sc.name == "unstarted" || sc.name == "refused-after-binding" {
						// The bound version becomes non-current, so I6 can see it.
						if _, err := w.pol().Rotate(synthTok(108), w.bindings()); err != nil {
							t.Fatal(err)
						}
					}
					v := w.violations(t, sc, false, firstAttempts)
					w.lapse(t)
					ctx := context.Background()
					for round := 0; round < 2; round++ {
						if err := newCrashWorld(w.root, w.st, runner, false).ticks(ctx, 4); err != nil {
							t.Fatalf("restart: %v", err)
						}
						w.lapse(t)
					}
					v = append(v, w.violations(t, sc, true, firstAttempts)...)
					key := sc.name + "/" + pt
					outcome := "ok"
					if len(v) > 0 {
						outcome = strings.Join(v, "; ")
						if why, ok := crashKnown[key]; ok && !strict {
							knownSeen[key] = true
							t.Logf("KNOWN %s: %s (%s)", key, outcome, why)
							outcome = "KNOWN: " + outcome
						} else {
							t.Errorf("%s: %s", key, outcome)
							outcome = "FAIL: " + outcome
						}
					}
					report = append(report, fmt.Sprintf("| %s | %s | died | %s |", sc.name, pt, outcome))
				})
			}
		})
	}
	if len(only) == 0 {
		var missed []string
		for _, p := range crashpoint.Table {
			if !reached[p.Name] && p.Path != "release" {
				missed = append(missed, p.Name)
			}
		}
		if len(missed) > 0 {
			t.Errorf("table points no scenario reaches: %s", strings.Join(missed, ", "))
		}
		for k := range crashKnown {
			if !knownSeen[k] && !strict {
				t.Errorf("known failure %s no longer fails: remove it", k)
			}
		}
	}
	sort.Strings(report)
	if out := os.Getenv(crashEnvReport); out != "" {
		body := "| Scenario | Point:pos:n | Child | Invariants I1 I2 I4 I5 I6 |\n|---|---|---|---|\n" + strings.Join(report, "\n") + "\n"
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
