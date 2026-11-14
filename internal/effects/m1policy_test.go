package effects

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// M1 comment policy tests (goodqa#15966 AC-7, AC-32). Store-backed: they
// need FABRIC_TEST_DSN like every broker test.

func setMarker(t *testing.T, ctx context.Context, st *store.Store, value string) {
	t.Helper()
	if _, err := st.Pool().Exec(ctx, `INSERT INTO fabric_settings (key, value) VALUES ('comment_policy', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatal(err)
	}
}

func canon(body string) IntentSpec {
	s := spec(M1Key("m_1"), body)
	return s
}

func effectCount(t *testing.T, ctx context.Context, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM effects WHERE mission_id = 'm_1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// noReadBroker is a broker whose connections cannot read fabric_settings
// (the revoked-SELECT row of the table).
func noReadBroker(t *testing.T, ctx context.Context, st *store.Store, m1 bool) *Broker {
	t.Helper()
	for _, q := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'w2_noread') THEN CREATE ROLE w2_noread NOLOGIN; END IF; END $$`,
		`GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO w2_noread`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO w2_noread`,
		`REVOKE ALL ON fabric_settings FROM w2_noread`,
	} {
		if _, err := st.Pool().Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	dsn := testDSN
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	nst, err := store.Open(ctx, dsn+sep+"options=-c%20role%3Dw2_noread")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nst.Close)
	b := New(nst)
	b.m1 = m1
	return b
}

// AC-7(6): a 62-character mission id yields a valid key; the key is
// deterministic and fits the trailer grammar.
func TestM1KeyShape(t *testing.T) {
	m := domain.MissionID("m_" + strings.Repeat("a1", 30))
	if len(m) != 62 {
		t.Fatal(len(m))
	}
	k := M1Key(m)
	if !regexp.MustCompile(`^m1-[0-9a-f]{24}$`).MatchString(k) || !idemKeyRE.MatchString(k) || k != M1Key(m) {
		t.Fatalf("key %q", k)
	}
	if M1Key("m_1") == M1Key("m_2") {
		t.Fatal("collision")
	}
	// GoodQA.Validate accepts it.
	g := &GoodQA{}
	req := []byte(`{"issue_id":"7c69d4c8-6ea2-4536-95db-6c2d6555c90d","ticket":"goodqa#1","body":"x"}`)
	if err := g.Validate(IntentSpec{IdemKey: k, Request: req}); err != nil {
		t.Fatal(err)
	}
}

func TestQuietWindowNeverBelowFloor(t *testing.T) {
	if QuietWindow() < 15*time.Minute || QuietWindow() < 2*goodqaCommentServerTimeout {
		t.Fatalf("quiet window %v", QuietWindow())
	}
}

// AC-7(3): every row of the build-and-marker table, for a governed Submit
// (canonical and not) and the start check.
func TestPolicyTable(t *testing.T) {
	type want struct {
		canonOK    bool   // canonical Submit succeeds
		nonCanonOK bool   // non-canonical Submit succeeds
		refusal    string // code of the refusal when refused ("" = db error)
		start      string // CheckPolicy refusal code, "ok", or "dberr"
	}
	rows := []struct {
		name   string
		m1     bool
		marker string // "", "m1", "other", "readerr"
		w      want
	}{
		{"m1 build, marker m1", true, "m1", want{true, false, CodeNonCanonical, "ok"}},
		{"m1 build, marker absent", true, "", want{false, false, CodeMarkerMissing, CodeMarkerMissing}},
		{"m1 build, marker other", true, "other", want{false, false, CodePolicyUnknown, CodePolicyUnknown}},
		{"m1 build, read error", true, "readerr", want{false, false, "", "dberr"}},
		{"non-m1 build, marker absent", false, "", want{true, true, "", "ok"}},
		{"non-m1 build, marker m1", false, "m1", want{true, false, CodeNonCanonical, "ok"}},
		{"non-m1 build, marker other", false, "other", want{false, false, CodePolicyUnknown, "ok"}},
		{"non-m1 build, read error", false, "readerr", want{false, false, "", "dberr"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx, st, b, fake := setup(t)
			b.m1 = r.m1
			if r.marker == "m1" || r.marker == "other" {
				setMarker(t, ctx, st, map[string]string{"m1": "m1", "other": "m2"}[r.marker])
			}
			if r.marker == "readerr" {
				b = noReadBroker(t, ctx, st, r.m1)
			}
			check := func(s IntentSpec, ok bool) {
				t.Helper()
				before := effectCount(t, ctx, st)
				_, err := b.Submit(ctx, fake, s)
				if ok {
					if err != nil {
						t.Fatalf("Submit %s: %v", s.IdemKey, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("Submit %s accepted", s.IdemKey)
				}
				if r.w.refusal == "" && IsPolicyRefusal(err, "") {
					t.Fatalf("want a database error, got refusal %v", err)
				}
				if r.w.refusal != "" && !IsPolicyRefusal(err, r.w.refusal) && !(ok == false && IsPolicyRefusal(err, CodeSecondEffect)) {
					t.Fatalf("Submit %s: %v, want %s", s.IdemKey, err, r.w.refusal)
				}
				if effectCount(t, ctx, st) != before {
					t.Fatal("a refused Submit changed the effects table")
				}
			}
			check(spec("k-other", "noncanon"), r.w.nonCanonOK)
			if r.w.nonCanonOK {
				// today's behaviour: a canonical key is just another key, so
				// the canonical Submit also succeeds (no policy).
				check(canon("canon"), true)
			} else {
				check(canon("canon"), r.w.canonOK)
			}
			err := b.CheckPolicy(ctx)
			switch r.w.start {
			case "ok":
				if err != nil {
					t.Fatalf("CheckPolicy: %v", err)
				}
			case "dberr":
				if err == nil || IsPolicyRefusal(err, "") {
					t.Fatalf("CheckPolicy: %v, want a database error", err)
				}
			default:
				if !IsPolicyRefusal(err, r.w.start) {
					t.Fatalf("CheckPolicy: %v, want %s", err, r.w.start)
				}
			}
		})
	}
}

// Other effect kinds are untouched by an unknown marker on a non-M1 build.
func TestPolicyLeavesOtherKinds(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m2")
	other := &kindFake{Fake: fake, kind: "test_other"}
	s := spec("k-x", "x")
	s.Kind = "test_other"
	e, err := b.Submit(ctx, other, s)
	if err != nil {
		t.Fatal(err)
	}
	if e, err = b.Dispatch(ctx, other, e.ID); err != nil || e.State != Confirmed {
		t.Fatalf("%s %v", e.State, err)
	}
}

type kindFake struct {
	*Fake
	kind string
}

func (k *kindFake) Kind() string { return k.kind }

// A second governed effect is refused even when the first was written by
// something that did not enforce the policy (a pre-marker row).
func TestSecondEffectRefused(t *testing.T) {
	ctx, st, b, fake := setup(t)
	if _, err := b.Submit(ctx, fake, spec("k-foreign", "foreign")); err != nil {
		t.Fatal(err)
	}
	setMarker(t, ctx, st, "m1")
	if _, err := b.Submit(ctx, fake, canon("mine")); !IsPolicyRefusal(err, CodeSecondEffect) {
		t.Fatalf("got %v, want %s", err, CodeSecondEffect)
	}
	// A replay of the existing row by its key changes nothing, as today.
	if e, err := b.Submit(ctx, fake, spec("k-foreign", "foreign")); err != nil || !e.Replayed {
		t.Fatalf("replay: %+v %v", e, err)
	}
	if effectCount(t, ctx, st) != 1 {
		t.Fatal("rows changed")
	}
}

// Concurrent canonical Submits from separate brokers: one row.
func TestConcurrentCanonicalSubmitsOneRow(t *testing.T) {
	ctx, st, _, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s2, err := store.Open(ctx, testDSN)
			if err != nil {
				t.Error(err)
				return
			}
			defer s2.Close()
			New(s2).Submit(ctx, fake, canon("same")) //nolint:errcheck
		}()
	}
	wg.Wait()
	if n := effectCount(t, ctx, st); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// AC-7(4): Retry and Dispatch refuse a ticket_comment row inserted with a
// non-canonical key.
func TestRetryDispatchRefuseNonCanonicalRow(t *testing.T) {
	ctx, st, b, fake := setup(t)
	e, err := b.Submit(ctx, fake, spec("k-raw", "raw"))
	if err != nil {
		t.Fatal(err)
	}
	fake.PreconditionFail = true
	if e, err = b.Dispatch(ctx, fake, e.ID); err != nil || e.State != FailedDefinite || e.Dispatches != 0 {
		t.Fatalf("setup: %s %v d=%d", e.State, err, e.Dispatches)
	}
	setMarker(t, ctx, st, "m1")
	if _, err := b.Retry(ctx, e.ID); !IsPolicyRefusal(err, CodeNonCanonical) {
		t.Fatalf("retry: %v", err)
	}
	e2, err := b.Submit(ctx, fake, spec("k-raw2", "raw2"))
	if err == nil {
		t.Fatalf("submit of a second non-canonical row accepted: %s", e2.ID)
	}
	// Dispatch of a non-canonical INTENT inserted before the marker.
	if _, err := st.Pool().Exec(ctx, `DELETE FROM fabric_settings WHERE key = 'comment_policy'`); err != nil {
		t.Fatal(err)
	}
	e3, err := b.Submit(ctx, fake, spec("k-raw3", "raw3"))
	if err != nil {
		t.Fatal(err)
	}
	setMarker(t, ctx, st, "m1")
	fake.PreconditionFail = false
	if _, err := b.Dispatch(ctx, fake, e3.ID); !IsPolicyRefusal(err, CodeNonCanonical) {
		t.Fatalf("dispatch: %v", err)
	}
	if fake.Sends() != 0 {
		t.Fatalf("%d sends", fake.Sends())
	}
}

// AC-32(1): once dispatched, a governed effect is never re-armed or
// re-dispatched, from FAILED_DEFINITE, UNKNOWN, NOT_APPLIED (after the quiet
// window), QUARANTINED, CONFIRMED and the RESOLVED states, and the
// CLI-style identical-key replay changes nothing.
func TestNoRedispatchFromEveryState(t *testing.T) {
	refused := func(t *testing.T, ctx context.Context, b *Broker, fake *Fake, id, where string) {
		t.Helper()
		if _, err := b.Retry(ctx, id); !IsPolicyRefusal(err, CodeNoRedispatch) {
			t.Fatalf("%s: Retry %v", where, err)
		}
		if _, err := b.Dispatch(ctx, fake, id); !IsPolicyRefusal(err, CodeNoRedispatch) {
			t.Fatalf("%s: Dispatch %v", where, err)
		}
		if r, err := b.Submit(ctx, fake, canon("once")); err != nil || !r.Replayed {
			t.Fatalf("%s: replay %+v %v", where, r, err)
		}
		if r, _ := b.Load(ctx, id); r.Dispatches != 1 {
			t.Fatalf("%s: dispatches %d, want 1", where, r.Dispatches)
		}
	}
	start := func(t *testing.T) (context.Context, *Broker, *Fake, Effect) {
		ctx, st, b, fake := setup(t)
		setMarker(t, ctx, st, "m1")
		b.SetTransportTimeout(time.Second)
		e, err := b.Submit(ctx, fake, canon("once"))
		if err != nil {
			t.Fatal(err)
		}
		return ctx, b, fake, e
	}
	t.Run("failed definite after transmission", func(t *testing.T) {
		ctx, b, fake, e := start(t)
		fake.Reject = true
		if e, _ = b.Dispatch(ctx, fake, e.ID); e.State != FailedDefinite || e.Dispatches != 1 {
			t.Fatalf("setup %s", e.State)
		}
		refused(t, ctx, b, fake, e.ID, "FAILED_DEFINITE")
	})
	t.Run("unknown then not applied", func(t *testing.T) {
		ctx, b, fake, e := start(t)
		fake.LoseBefor = true
		if e, _ = b.Dispatch(ctx, fake, e.ID); e.State != Unknown {
			t.Fatalf("setup %s", e.State)
		}
		fake.LoseBefor = false
		refused(t, ctx, b, fake, e.ID, "UNKNOWN")
		if got, err := b.Reconcile(ctx, fake, e.ID); err != nil || got.State != Unknown {
			t.Fatalf("inside window: %s %v", got.State, err)
		}
		b.quietWindow = 0
		time.Sleep(1100 * time.Millisecond)
		if got, err := b.Reconcile(ctx, fake, e.ID); err != nil || got.State != NotApplied {
			t.Fatalf("after window: %s %v", got.State, err)
		}
		refused(t, ctx, b, fake, e.ID, "NOT_APPLIED after the quiet window")
		if fake.Sends() != 0 {
			t.Fatalf("sends %d", fake.Sends())
		}
	})
	t.Run("quarantined and resolved", func(t *testing.T) {
		ctx, b, fake, e := start(t)
		fake.LoseAfter = true
		if e, _ = b.Dispatch(ctx, fake, e.ID); e.State != Unknown {
			t.Fatalf("setup %s", e.State)
		}
		fake.mu.Lock()
		fake.applied[e.IdemKey] = []byte(`{"other":"body"}`)
		fake.mu.Unlock()
		if got, err := b.Reconcile(ctx, fake, e.ID); err != nil || got.State != Quarantined {
			t.Fatalf("reconcile: %s %v", got.State, err)
		}
		refused(t, ctx, b, fake, e.ID, "QUARANTINED")
		if _, err := b.ResolveQuarantined(ctx, e.ID, ResolvedNotApplied, "test", "wal:x", "op:test"); err != nil {
			t.Fatal(err)
		}
		refused(t, ctx, b, fake, e.ID, "RESOLVED_NOT_APPLIED")
		if fake.Sends() != 1 {
			t.Fatalf("sends %d, want 1", fake.Sends())
		}
	})
	t.Run("confirmed", func(t *testing.T) {
		ctx, b, fake, e := start(t)
		if e, _ = b.Dispatch(ctx, fake, e.ID); e.State != Confirmed {
			t.Fatalf("setup %s", e.State)
		}
		refused(t, ctx, b, fake, e.ID, "CONFIRMED")
	})
}

// AC-7(7): an empty readback inside the window leaves UNKNOWN; after it,
// NOT_APPLIED. A non-governed policy (unmarked non-M1) is unchanged.
func TestReadbackGuardWindow(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	b.SetTransportTimeout(time.Second)
	e, _ := b.Submit(ctx, fake, canon("g"))
	fake.LoseBefor = true
	e, err := b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Unknown {
		t.Fatalf("%s %v", e.State, err)
	}
	if e, err = b.Reconcile(ctx, fake, e.ID); err != nil || e.State != Unknown || !strings.Contains(e.LastError, "quiet window") {
		t.Fatalf("inside: %s %v %q", e.State, err, e.LastError)
	}
	b.quietWindow = 500 * time.Millisecond
	time.Sleep(1600 * time.Millisecond)
	if e, err = b.Reconcile(ctx, fake, e.ID); err != nil || e.State != NotApplied {
		t.Fatalf("after: %s %v", e.State, err)
	}

	// Unmarked non-M1: today's behaviour, NOT_APPLIED at once.
	ctx, _, b, fake = setup(t)
	b.SetTransportTimeout(time.Second)
	e, _ = b.Submit(ctx, fake, spec("k-u", "u"))
	fake.LoseBefor = true
	e, _ = b.Dispatch(ctx, fake, e.ID)
	if e, err = b.Reconcile(ctx, fake, e.ID); err != nil || e.State != NotApplied {
		t.Fatalf("unmarked: %s %v", e.State, err)
	}
}

// The guard also holds on an M1 build whose marker is missing.
func TestReadbackGuardOnM1BuildWithoutMarker(t *testing.T) {
	ctx, _, b, fake := setup(t)
	b.SetTransportTimeout(time.Second)
	e, _ := b.Submit(ctx, fake, spec("k-u", "u"))
	fake.LoseBefor = true
	e, _ = b.Dispatch(ctx, fake, e.ID)
	b.m1 = true
	if e, err := b.Reconcile(ctx, fake, e.ID); err != nil || e.State != Unknown {
		t.Fatalf("%s %v", e.State, err)
	}
}

// AC-32(4): a precondition-refused effect (dispatches = 0) is retried and
// then dispatched once.
func TestPreconditionRefusedRetriesOnce(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	e, _ := b.Submit(ctx, fake, canon("p"))
	fake.PreconditionFail = true
	e, err := b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != FailedDefinite || e.Dispatches != 0 {
		t.Fatalf("%s %v", e.State, err)
	}
	fake.PreconditionFail = false
	if e, err = b.Retry(ctx, e.ID); err != nil || e.State != Intent {
		t.Fatalf("retry: %s %v", e.State, err)
	}
	if e, err = b.Dispatch(ctx, fake, e.ID); err != nil || e.State != Confirmed || fake.Sends() != 1 {
		t.Fatalf("dispatch: %s %v sends=%d", e.State, err, fake.Sends())
	}
	if _, err := b.Retry(ctx, e.ID); err == nil {
		t.Fatal("retry of a confirmed effect")
	}
}

// AC-32(3): two concurrent Dispatch calls from separate brokers (separate
// pools) on one governed INTENT: exactly one transmits.
func TestConcurrentDispatchOneTransmission(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	e, _ := b.Submit(ctx, fake, canon("c"))
	fake.Stall = 200 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s2, err := store.Open(ctx, testDSN)
			if err != nil {
				t.Error(err)
				return
			}
			defer s2.Close()
			New(s2).Dispatch(ctx, fake, e.ID) //nolint:errcheck
		}()
	}
	wg.Wait()
	if fake.Sends() != 1 {
		t.Fatalf("sends %d, want 1", fake.Sends())
	}
}

// AC-32(6): a dispatch interleaved between Retry's read and its re-arm must
// not leave a re-armed row with dispatches > 0. Retry reads the row FOR
// UPDATE in the re-arming transaction, so the interleaved Retry+Dispatch
// waits. A variant that reads outside that transaction fails this test.
func TestRetryInterleavedDispatch(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	e, _ := b.Submit(ctx, fake, canon("r"))
	fake.PreconditionFail = true
	if e, _ = b.Dispatch(ctx, fake, e.ID); e.State != FailedDefinite || e.Dispatches != 0 {
		t.Fatalf("setup %s", e.State)
	}
	s2, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	other := New(s2)
	done := make(chan struct{})
	b.beforeRearm = func(Effect) {
		b.beforeRearm = nil
		go func() {
			defer close(done)
			fake.mu.Lock()
			fake.PreconditionFail = false
			fake.Reject = true
			fake.mu.Unlock()
			if _, err := other.Retry(ctx, e.ID); err == nil {
				other.Dispatch(ctx, fake, e.ID) //nolint:errcheck
			} else {
				other.Dispatch(ctx, fake, e.ID) //nolint:errcheck
			}
		}()
		time.Sleep(500 * time.Millisecond) // let it run, or block on our row lock
	}
	if _, err := b.Retry(ctx, e.ID); err != nil && !IsPolicyRefusal(err, "") && !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry: %v", err)
	}
	<-done
	cur, err := b.Load(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if (cur.State == Intent || cur.State == Approved) && cur.Dispatches > 0 {
		t.Fatalf("re-armed a dispatched governed effect: %s dispatches=%d", cur.State, cur.Dispatches)
	}
	// Whatever happens next, no second transmission is possible.
	b.Dispatch(ctx, fake, e.ID) //nolint:errcheck
	if cur, _ := b.Load(ctx, e.ID); cur.Dispatches > 1 {
		t.Fatalf("dispatches %d", cur.Dispatches)
	}
}

// AC-32(7): no regression. Unmarked database, non-M1 build: dispatched
// NOT_APPLIED and FAILED_DEFINITE effects re-arm and dispatch again.
func TestNoRegressionUnmarked(t *testing.T) {
	ctx, _, b, fake := setup(t)
	b.SetTransportTimeout(time.Second)
	fd, _ := b.Submit(ctx, fake, spec("k-fd", "fd"))
	fake.Reject = true
	fd, _ = b.Dispatch(ctx, fake, fd.ID)
	na, _ := b.Submit(ctx, fake, spec("k-na", "na"))
	fake.Reject = false
	fake.LoseBefor = true
	na, _ = b.Dispatch(ctx, fake, na.ID)
	na, _ = b.Reconcile(ctx, fake, na.ID)
	if fd.State != FailedDefinite || na.State != NotApplied {
		t.Fatalf("setup %s %s", fd.State, na.State)
	}
	fake.LoseBefor = false
	for _, e := range []Effect{fd, na} {
		r, err := b.Retry(ctx, e.ID)
		if err != nil {
			t.Fatalf("retry %s: %v", e.IdemKey, err)
		}
		if r, err = b.Dispatch(ctx, fake, r.ID); err != nil || r.State != Confirmed || r.Dispatches != 2 {
			t.Fatalf("redispatch %s: %s %v d=%d", e.IdemKey, r.State, err, r.Dispatches)
		}
	}
}

// AC-7(5) in-package: a key with the m1- prefix that is not the mission's
// canonical key is refused; classification never matches a prefix.
func TestPrefixKeyIsNotCanonical(t *testing.T) {
	ctx, st, b, fake := setup(t)
	setMarker(t, ctx, st, "m1")
	if _, err := b.Submit(ctx, fake, spec("m1-000000000000000000000000", "p")); !IsPolicyRefusal(err, CodeNonCanonical) {
		t.Fatalf("got %v", err)
	}
	if _, err := b.Submit(ctx, fake, spec(M1Key("m_2"), "p")); !IsPolicyRefusal(err, CodeNonCanonical) {
		t.Fatalf("another mission's key: %v", err)
	}
}
