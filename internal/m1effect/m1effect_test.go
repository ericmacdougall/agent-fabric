package m1effect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/effects"
	"fabric/internal/m1comment"
	"fabric/internal/records"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "m1e")) }

// noCreds is a bound credential set with nothing to find; tokenCreds finds
// one synthetic value.
type tokenCreds struct{ token string }

func (c tokenCreds) ScanText(text string) (bool, string) {
	if c.token != "" && strings.Contains(text, c.token) {
		return true, "credential_form:token:v1:raw"
	}
	return false, ""
}
func (c tokenCreds) ScanTextExcluding(text string, _ []string) (bool, string) {
	return c.ScanText(text)
}

type source struct {
	result string
	creds  m1comment.BoundCredentials
	err    error
}

func (s source) Prepare(ctx context.Context, a Attempt) (Delivery, error) {
	if s.err != nil {
		return Delivery{}, s.err
	}
	return Delivery{IssueID: "7c69d4c8-6ea2-4536-95db-6c2d6555c90d", Ticket: "goodqa#15966",
		Fields: m1comment.Fields{Mission: string(a.Mission), Provider: "claude-code", ClaudeSHA: strings.Repeat("ab", 32), Model: "claude-x"},
		Result: s.result, Allow: m1comment.Allow{Mission: string(a.Mission), Attempt: string(a.Attempt)}, Creds: s.creds}, nil
}

const mission = domain.MissionID("m_e")

type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	b     *effects.Broker
	fake  *effects.Fake
	step  *Step
	loop  *Loop
	dir   string
	n     int // attempts started
	tries int // admission tries: each gets its own command id (a refusal is stored and replays)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	st := storetest.Open(t)
	if err := st.CreateMission(ctx, mission, domain.ProviderClaudeCode, "GoodQA goodqa#15966", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')`); err != nil {
		t.Fatal(err)
	}
	b := effects.New(st)
	b.SetTransportTimeout(time.Second)
	fake := effects.NewFake()
	dir := t.TempDir()
	e := &env{t: t, ctx: ctx, st: st, b: b, fake: fake, dir: dir}
	e.step = &Step{Store: st, Broker: b, Exec: fake, Source: source{result: "test.sh fails: off-by-one in sum_to", creds: tokenCreds{}}, RecordsDir: dir}
	e.loop = &Loop{Store: st, Broker: b, Exec: fake, RecordsDir: dir}
	e.exec("admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: mission}, "fabd")
	return e
}

func (e *env) exec(key string, cmd domain.Command, actor domain.ActorID) error {
	e.t.Helper()
	_, err := e.st.Execute(e.ctx, actor, domain.CommandID(key), cmd)
	return err
}

// start is the scheduler's START_ATTEMPT for the next attempt.
func (e *env) start() (Attempt, error) {
	e.n++
	id := domain.AttemptID(fmt.Sprintf("r_%d", e.n))
	resp, err := e.st.Execute(e.ctx, "fabd", domain.CommandID("start-"+string(id)), domain.Command{Kind: domain.CmdStartAttempt,
		Path: domain.ProviderClaudeCode, Mission: mission, Attempt: id, Host: "h_t", Role: "writer"})
	if err != nil {
		return Attempt{}, err
	}
	return Attempt{Mission: mission, Attempt: id, Fence: resp.Events[0].Fence, Outcome: "checkpoint"}, nil
}

// admitNext is the real admission path after an attempt ended: unpause a
// paused mission (operator), admit an idle one, then START_ATTEMPT.
func (e *env) admitNext() (Attempt, error) {
	m, err := e.st.Mission(e.ctx, mission)
	if err != nil {
		return Attempt{}, err
	}
	e.tries++
	switch m.Execution {
	case domain.ExecPaused:
		if err := e.exec(fmt.Sprintf("unpause-%d", e.tries), domain.Command{Kind: domain.CmdUnpauseMission, Mission: mission, Reason: "test"}, "op:test"); err != nil {
			return Attempt{}, err
		}
	case domain.ExecAdmitted, domain.ExecRunning:
	default:
		if err := e.exec(fmt.Sprintf("admit-%d", e.tries), domain.Command{Kind: domain.CmdAdmitMission, Mission: mission}, "op:test"); err != nil {
			return Attempt{}, err
		}
	}
	return e.start()
}

func (e *env) finish(a Attempt, reason string) {
	e.t.Helper()
	if err := e.exec("finish:"+string(a.Attempt), domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission, Attempt: a.Attempt,
		Fence: a.Fence, Outcome: "checkpoint", Reason: reason}, "fabd"); err != nil {
		e.t.Fatal(err)
	}
	// fabd's finishCmd verifies a non-abandoned finish.
	if err := e.exec("verify:"+string(a.Attempt), domain.Command{Kind: domain.CmdReconcileAttempt, Mission: mission, Attempt: a.Attempt,
		Fence: a.Fence, Result: "VERIFIED"}, "fabd"); err != nil {
		e.t.Fatal(err)
	}
}

// crashEnd is fabd dying mid-attempt and its restart: the lease lapses,
// the sweep marks the attempt UNKNOWN and the gone runner is quarantined.
func (e *env) crashEnd(a Attempt) {
	e.t.Helper()
	if _, err := e.st.Pool().Exec(e.ctx, `UPDATE attempts SET lease_until = now() - interval '1 second' WHERE id = $1`, string(a.Attempt)); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.ExpireLeases(e.ctx, 50); err != nil {
		e.t.Fatal(err)
	}
	if err := e.exec("reconcile:gone:"+string(a.Attempt), domain.Command{Kind: domain.CmdReconcileAttempt, Mission: mission, Attempt: a.Attempt,
		Fence: a.Fence, Result: "QUARANTINED", Reason: "runner gone"}, "fabd"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) effect() (effects.Effect, bool) {
	x, err := e.b.LoadKey(e.ctx, effects.M1Key(mission))
	return x, err == nil
}

func (e *env) settled() bool {
	x, ok := e.effect()
	if !ok {
		return true
	}
	switch x.State {
	case effects.Confirmed, effects.FailedDefinite, effects.NotApplied, effects.ResolvedApplied, effects.ResolvedNotApplied:
		return true
	}
	return false
}

// settle runs AC-31 passes until the effect is settled (bounded).
func (e *env) settle() {
	e.t.Helper()
	for i := 0; i < 20 && !e.settled(); i++ {
		if _, err := e.loop.Pass(e.ctx); err != nil {
			e.t.Fatal(err)
		}
		if !e.settled() {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if _, err := e.loop.Pass(e.ctx); err != nil { // records for the settled state
		e.t.Fatal(err)
	}
}

func (e *env) blockedRecords() []records.Record {
	rs, err := records.List(e.dir, records.DeliveryBlocked)
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

// assertEnd: exactly one of the two AC-8 end states, on the remote count
// and the local effect state; never two comments, never two sends.
func (e *env) assertEnd() string {
	e.t.Helper()
	x, has := e.effect()
	comments := e.fake.Count()
	if e.fake.Sends() > 1 {
		e.t.Fatalf("%d transmissions", e.fake.Sends())
	}
	var n int
	e.st.Pool().QueryRow(e.ctx, `SELECT count(*) FROM effects WHERE mission_id = $1`, string(mission)).Scan(&n) //nolint:errcheck
	if n > 1 {
		e.t.Fatalf("%d effect rows", n)
	}
	switch {
	case comments == 1 && has && (x.State == effects.Confirmed || x.State == effects.ResolvedApplied):
		return "confirmed"
	case comments == 0 && len(e.blockedRecords()) > 0:
		return "blocked"
	}
	e.t.Fatalf("bad end state: comments=%d effect=%v/%s blocked=%d", comments, has, x.State, len(e.blockedRecords()))
	return ""
}

func isRefusal(err error) bool {
	var r *store.Refusal
	return errors.As(err, &r)
}

// ---- AC-7 normal path, AC-11, AC-29 ----

func TestStepDeliversOnce(t *testing.T) {
	e := newEnv(t)
	a, err := e.start()
	if err != nil {
		t.Fatal(err)
	}
	r := e.step.Run(e.ctx, a, func(ctx context.Context) error { return e.st.Heartbeat(ctx, a.Attempt, a.Fence) })
	if r.Kind != Delivered || r.Prefix() != "" {
		t.Fatalf("%+v", r)
	}
	e.finish(a, r.Prefix())
	// rule (b): a later attempt records delivered_earlier and sends nothing.
	b, err := e.admitNext()
	if err != nil {
		t.Fatal(err)
	}
	if r := e.step.Run(e.ctx, b, nil); r.Kind != DeliveredEarlier {
		t.Fatalf("later attempt: %+v", r)
	}
	rs, _ := records.Read(e.dir, b.Attempt)
	if len(rs) != 1 || rs[0].Kind != records.DeliveredEarlier {
		t.Fatalf("records %+v", rs)
	}
	if e.assertEnd() != "confirmed" || e.fake.Sends() != 1 {
		t.Fatal("not one confirmed comment")
	}
}

func TestStepSkipsWithoutCheckpoint(t *testing.T) {
	e := newEnv(t)
	a, _ := e.start()
	a.Outcome = "abandoned"
	if r := e.step.Run(e.ctx, a, nil); r.Kind != Skipped {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.effect(); ok {
		t.Fatal("submitted without an EndCheckpoint")
	}
}

// AC-11: less than 120 s of lease left refuses the step, nothing is
// submitted, delivery_blocked lease_budget is recorded and the FINISH prefix
// is m1:delivery_blocked:lease_budget.
func TestStepLeaseBudget(t *testing.T) {
	e := newEnv(t)
	a, _ := e.start()
	e.st.Pool().Exec(e.ctx, `UPDATE attempts SET lease_until = now() + interval '90 seconds' WHERE id = $1`, string(a.Attempt)) //nolint:errcheck
	r := e.step.Run(e.ctx, a, nil)
	if r.Kind != Blocked || r.Class != "lease_budget" || r.Prefix() != "m1:delivery_blocked:lease_budget" {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.effect(); ok || e.fake.Sends() != 0 {
		t.Fatal("submitted with a short lease")
	}
	// The heartbeat before the step renews a short lease (store lease 5 min).
	hb := func(ctx context.Context) error { return e.st.Heartbeat(ctx, a.Attempt, a.Fence) }
	if r := e.step.Run(e.ctx, a, hb); r.Kind != Delivered {
		t.Fatalf("after heartbeat: %+v", r)
	}
}

// AC-10 via the step, and AC-29: a blocked body leaves no body or credential
// bytes in the record, nothing is submitted.
func TestStepScanBlocksAndRecordHasNoBody(t *testing.T) {
	e := newEnv(t)
	tok := "SYNTHETICtokenVALUE0123456789abcdefXYZ"
	e.step.Source = source{result: "the token is " + tok + " and here is a diagnosis", creds: tokenCreds{token: tok}}
	a, _ := e.start()
	r := e.step.Run(e.ctx, a, nil)
	if r.Kind != Blocked || !strings.HasPrefix(r.Class, "scan_credential") {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.effect(); ok || e.fake.Sends() != 0 {
		t.Fatal("submitted a blocked body")
	}
	raw, err := os.ReadFile(records.Path(e.dir, a.Attempt))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), "diagnosis") || strings.Contains(string(raw), tok[:12]) {
		t.Fatalf("record holds body bytes: %s", raw)
	}
}

func TestStepSourceClassError(t *testing.T) {
	e := newEnv(t)
	e.step.Source = source{err: &ClassError{Class: "credential_material_unavailable", Err: errors.New("binding record missing")}}
	a, _ := e.start()
	if r := e.step.Run(e.ctx, a, nil); r.Class != "credential_material_unavailable" {
		t.Fatalf("%+v", r)
	}
}

// A foreign ticket_comment row (written before the marker): Submit refuses
// m1_second_effect and fabd records delivery_blocked with that class.
func TestStepForeignRowBlocks(t *testing.T) {
	e := newEnv(t)
	a, _ := e.start()
	e.st.Pool().Exec(e.ctx, `DELETE FROM fabric_settings WHERE key = 'comment_policy'`) //nolint:errcheck
	req := []byte(`{"ticket":"goodqa#1","body":"foreign"}`)
	legacy := effects.New(e.st) // a non-M1 broker, also in an M1 test build
	legacy.TestAsNonM1()
	if _, err := legacy.Submit(e.ctx, e.fake, effects.IntentSpec{Mission: mission, Attempt: a.Attempt, Fence: a.Fence, Kind: "ticket_comment",
		Subject: "ticket:goodqa#1", IdemKey: "k-foreign", Request: req}); err != nil {
		t.Fatal(err)
	}
	e.st.Pool().Exec(e.ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')`) //nolint:errcheck
	if r := e.step.Run(e.ctx, a, nil); r.Kind != Blocked || r.Class != effects.CodeSecondEffect {
		t.Fatalf("%+v", r)
	}
	if e.fake.Sends() != 0 {
		t.Fatal("sent")
	}
}

// ---- AC-8 cutpoints ----

type crash struct{}

func crashAt() { panic(crash{}) }

func runCrashing(f func()) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	f()
	return false
}

func TestCutpoints(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		want  string
	}{
		{"before submit", func(e *env) { e.step.beforeSubmit = crashAt }, "confirmed"},
		{"after intent commit", func(e *env) { e.step.afterSubmit = func(effects.Effect) { crashAt() } }, "blocked"},
		{"after dispatched commit", func(e *env) { e.b.TestHooks(0, func(effects.Effect) { crashAt() }, nil) }, "blocked"},
		{"after transport before settle", func(e *env) { e.b.TestHooks(0, nil, func(effects.Effect, effects.Receipt) { crashAt() }) }, "confirmed"},
		{"after confirmed before finish", func(e *env) {}, "confirmed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			a, err := e.start()
			if err != nil {
				t.Fatal(err)
			}
			crashed := runCrashing(func() { e.step.Run(e.ctx, a, nil) })
			if c.name != "after confirmed before finish" && !crashed {
				t.Fatal("cutpoint not reached")
			}
			// fabd dies before FINISH; restart: lease expiry, quarantine.
			e.crashEnd(a)
			e.step.beforeSubmit, e.step.afterSubmit = nil, nil
			e.b.TestHooks(0, nil, nil)
			// Admission while the effect is unsettled is refused.
			if !e.settled() {
				if _, err := e.admitNext(); err == nil || !isRefusal(err) {
					t.Fatalf("admitted with an unsettled effect: %v", err)
				}
				time.Sleep(2100 * time.Millisecond) // past the stale-dispatch window (2 x 1 s)
			}
			e.settle()
			if !e.settled() {
				t.Fatal("AC-31 did not settle the effect")
			}
			// A later attempt runs and meets rule (a), (b) or (c).
			b, err := e.admitNext()
			if err != nil {
				x, _ := e.effect()
				var rows []string
				r2, _ := e.st.Pool().Query(e.ctx, `SELECT id, state, kind FROM effects`)
				for r2.Next() {
					var a, b2, c string
					r2.Scan(&a, &b2, &c) //nolint:errcheck
					rows = append(rows, a+":"+b2+":"+c)
				}
				r2.Close()
				t.Fatalf("admission after settle: %v (effect %s; rows %v)", err, x.State, rows)
			}
			r := e.step.Run(e.ctx, b, nil)
			e.finish(b, r.Prefix())
			if got := e.assertEnd(); got != c.want {
				t.Fatalf("end %s, want %s (later attempt %+v)", got, c.want, r)
			}
		})
	}
}

// AC-8 delay case 1 and AC-31: GoodQA holds the request; readback inside
// the quiet window is empty, so the effect stays UNKNOWN and admission is
// refused; the request is released and the loop confirms it, no CLI step.
func TestHeldRequestConfirmedByLoop(t *testing.T) {
	e := newEnv(t)
	e.fake.Hold = true
	a, _ := e.start()
	if r := e.step.Run(e.ctx, a, nil); r.Kind != Pending {
		t.Fatalf("%+v", r)
	}
	e.crashEnd(a)
	time.Sleep(2100 * time.Millisecond)
	if _, err := e.loop.Pass(e.ctx); err != nil {
		t.Fatal(err)
	}
	if x, _ := e.effect(); x.State != effects.Unknown {
		t.Fatalf("inside the window: %s", x.State)
	}
	if _, err := e.admitNext(); err == nil {
		t.Fatal("admitted while UNKNOWN")
	}
	e.fake.Release()
	e.settle()
	if x, _ := e.effect(); x.State != effects.Confirmed {
		t.Fatalf("after release: %s", x.State)
	}
	if e.assertEnd() != "confirmed" {
		t.Fatal("end")
	}
}

// AC-8 delay case 2: lost response. AC-31 confirms by trailer and a later
// attempt records delivered_earlier.
func TestLostResponseConfirmedThenDeliveredEarlier(t *testing.T) {
	e := newEnv(t)
	e.fake.LoseAfter = true
	a, _ := e.start()
	if r := e.step.Run(e.ctx, a, nil); r.Kind != Pending {
		t.Fatalf("%+v", r)
	}
	e.finish(a, "")
	e.fake.LoseAfter = false
	time.Sleep(2100 * time.Millisecond)
	e.settle()
	b, err := e.admitNext()
	if err != nil {
		t.Fatal(err)
	}
	if r := e.step.Run(e.ctx, b, nil); r.Kind != DeliveredEarlier {
		t.Fatalf("%+v", r)
	}
	if e.assertEnd() != "confirmed" || e.fake.Sends() != 1 {
		t.Fatal("end")
	}
}

// Owner addition (1): the effect is left UNKNOWN after a lost transport,
// the attempt finishes (mission back to ADMITTED), the next attempt starts
// and runs its pre-submit step: no transport call; the loop then reconciles
// the original effect; the readback ends at 1 comment or 0 plus
// delivery_blocked.
func TestNextAttemptWhileUnknownSendsNothing(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			e := newEnv(t)
			e.b.TestHooks(0, nil, nil)
			e.fake.LoseAfter, e.fake.LoseBefor = applied, !applied
			a, _ := e.start()
			if r := e.step.Run(e.ctx, a, nil); r.Kind != Pending {
				t.Fatalf("%+v", r)
			}
			e.finish(a, "")
			e.fake.LoseAfter, e.fake.LoseBefor = false, false
			sends := e.fake.Sends()
			b, err := e.admitNext()
			if err != nil {
				t.Fatalf("START_ATTEMPT is not gated by effects (spec erratum): %v", err)
			}
			r := e.step.Run(e.ctx, b, nil)
			if r.Kind != Blocked || r.Class != "effect_unknown" {
				t.Fatalf("pre-submit while UNKNOWN: %+v", r)
			}
			if e.fake.Sends() != sends {
				t.Fatal("the later attempt transmitted")
			}
			e.finish(b, r.Prefix())
			time.Sleep(2100 * time.Millisecond)
			e.settle()
			want := map[bool]string{true: "confirmed", false: "blocked"}[applied]
			if got := e.assertEnd(); got != want {
				t.Fatalf("end %s, want %s", got, want)
			}
		})
	}
}

// AC-31: a stranded never-dispatched INTENT of an ended attempt is
// abandoned (superseded) with delivery_blocked; the loop selects by the
// shared predicate with no mission config anywhere; a non-canonical
// ticket_comment row is not touched.
func TestLoopAbandonsStrandedIntent(t *testing.T) {
	e := newEnv(t)
	e.step.afterSubmit = func(effects.Effect) { crashAt() }
	a, _ := e.start()
	runCrashing(func() { e.step.Run(e.ctx, a, nil) })
	e.crashEnd(a)
	ps, err := e.loop.Pass(e.ctx)
	if err != nil || ps.Abandoned != 1 {
		t.Fatalf("%+v %v", ps, err)
	}
	x, _ := e.effect()
	if x.State != effects.NotApplied || !strings.Contains(x.LastError, "superseded") {
		t.Fatalf("%s %q", x.State, x.LastError)
	}
	rs, _ := records.Read(e.dir, a.Attempt)
	if len(rs) == 0 || rs[0].Class != "superseded" {
		t.Fatalf("records %+v", rs)
	}
}

// AC-31 late_write (RR-4): a write that lands after the quiet window leaves
// the effect NOT_APPLIED; the loop records late_write with the comment id
// and raises the metric once, without changing the state.
func TestLoopLateWrite(t *testing.T) {
	e := newEnv(t)
	e.b.TestHooks(0, nil, nil)
	e.fake.Hold = true
	e.loop.Recheck = time.Nanosecond
	bumps := 0
	e.loop.Bump = func(_ context.Context, name string) {
		if name == "m1_late_write_total" {
			bumps++
		}
	}
	a, _ := e.start()
	e.step.Run(e.ctx, a, nil)
	e.crashEnd(a)
	time.Sleep(2100 * time.Millisecond)
	e.settle()
	if x, _ := e.effect(); x.State != effects.NotApplied {
		t.Fatalf("%s", x.State)
	}
	e.fake.Release() // the late write
	for i := 0; i < 3; i++ {
		if _, err := e.loop.Pass(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := records.List(e.dir, records.LateWrite)
	if len(rs) != 1 || rs[0].CommentID == "" || bumps != 1 {
		t.Fatalf("late writes %+v bumps %d", rs, bumps)
	}
	if x, _ := e.effect(); x.State != effects.NotApplied {
		t.Fatalf("state changed to %s", x.State)
	}
}

// The loop runs only where the policy is enforced and selects only
// canonical governed rows.
func TestLoopPredicate(t *testing.T) {
	e := newEnv(t)
	e.st.Pool().Exec(e.ctx, `DELETE FROM fabric_settings WHERE key = 'comment_policy'`) //nolint:errcheck
	a, _ := e.start()
	req := []byte(`{"ticket":"goodqa#1","body":"x"}`)
	legacy := effects.New(e.st) // a non-M1 broker, also in an M1 test build
	legacy.TestAsNonM1()
	x, err := legacy.Submit(e.ctx, e.fake, effects.IntentSpec{Mission: mission, Attempt: a.Attempt, Fence: a.Fence, Kind: "ticket_comment",
		Subject: "ticket:goodqa#1", IdemKey: "k-legacy", Request: req})
	if err != nil {
		t.Fatal(err)
	}
	e.crashEnd(a)
	if ps, err := e.loop.Pass(e.ctx); err != nil || ps != (PassStats{}) {
		t.Fatalf("unmarked: %+v %v", ps, err)
	}
	e.st.Pool().Exec(e.ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', 'm1')`) //nolint:errcheck
	if ps, err := e.loop.Pass(e.ctx); err != nil || ps != (PassStats{}) {
		t.Fatalf("non-canonical row touched: %+v %v", ps, err)
	}
	if y, _ := e.b.Load(e.ctx, x.ID); y.State != effects.Intent {
		t.Fatalf("legacy row changed: %s", y.State)
	}
}

// AC-31 starvation: terminal rows that never change state (record-only
// FAILED_DEFINITE, and NOT_APPLIED inside the late-write window) are older
// than, and outnumber, a stranded INTENT. With Limit 2 the INTENT is still
// abandoned in the first pass, and a second pass does not redo the
// record-only rows or re-read the late-write ones.
func TestLoopOldTerminalRowsDoNotStarve(t *testing.T) {
	e := newEnv(t)
	e.loop.Limit = 2
	reads := 0
	for i := 0; i < 3; i++ {
		for _, st := range []string{"FAILED_DEFINITE", "NOT_APPLIED"} {
			m := domain.MissionID(fmt.Sprintf("m_old_%s_%d", strings.ToLower(st[:1]), i))
			if err := e.st.CreateMission(e.ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#15966", "fabric", "main"); err != nil {
				t.Fatal(err)
			}
			// Fixture only: seed settled rows directly, past the
			// born-as-intent trigger (replica role skips user triggers).
			tx, err := e.st.Pool().Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(e.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(e.ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, dispatches, updated_at)
				VALUES ($1, $2, 'r_gone', 1, 'ticket_comment', $1, $3, '{}', 'x', $4, 1, now() - interval '1 hour')`,
				"e_old_"+string(m), string(m), effects.M1Key(m), st); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.loop.Exec = countingExec{Executor: e.fake, n: &reads}
	e.step.afterSubmit = func(effects.Effect) { crashAt() }
	a, _ := e.start()
	runCrashing(func() { e.step.Run(e.ctx, a, nil) })
	e.crashEnd(a)
	ps, err := e.loop.Pass(e.ctx)
	if err != nil || ps.Abandoned != 1 {
		t.Fatalf("first pass %+v %v: the stranded INTENT starved", ps, err)
	}
	if x, _ := e.effect(); x.State != effects.NotApplied {
		t.Fatalf("intent %s", x.State)
	}
	first := reads
	ps, err = e.loop.Pass(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 3 rows of each kind, 2 done in the first pass: the third record-only
	// row, and one late-write read of the third NOT_APPLIED row (which also
	// writes its delivery_blocked).
	if ps.Blocked != 2 || reads != first+1 {
		t.Fatalf("second pass %+v reads %d->%d: done rows were not skipped", ps, first, reads)
	}
	if ps, err = e.loop.Pass(e.ctx); err != nil || ps != (PassStats{}) || reads != first+1 {
		t.Fatalf("third pass %+v %v reads %d: nothing is left to do", ps, err, reads)
	}
}

type countingExec struct {
	effects.Executor
	n *int
}

func (c countingExec) FindComment(ctx context.Context, e effects.Effect) (string, bool, error) {
	*c.n++
	return c.Executor.(effects.CommentFinder).FindComment(ctx, e)
}

// AC-31: a pass is bounded by its own deadline, so a hung GoodQA readback
// cannot stall the scheduler tick.
func TestLoopPassDeadline(t *testing.T) {
	e := newEnv(t)
	e.b.TestHooks(0, nil, nil)
	e.fake.LoseBefor = true
	a, _ := e.start()
	e.step.Run(e.ctx, a, nil)
	e.crashEnd(a)
	e.fake.Hang = true
	e.loop.Timeout = 300 * time.Millisecond
	t0 := time.Now()
	e.loop.Pass(e.ctx) //nolint:errcheck
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("pass took %v with a 300 ms deadline", d)
	}
}
