package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
	"fabric/internal/store"
)

var testDSN string

func TestMain(m *testing.M) {
	admin := os.Getenv("FABRIC_TEST_DSN")
	if admin == "" {
		fmt.Fprintln(os.Stderr, "FABRIC_TEST_DSN is not set: the broker tests require a disposable Postgres")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	name := fmt.Sprintf("fabric_fx_%d", os.Getpid())
	pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	testDSN = u.String()
	st, err := store.Open(ctx, testDSN)
	if err == nil {
		err = st.Migrate(ctx)
		st.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	code := m.Run()
	pool.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
	pool.Close()
	os.Exit(code)
}

// setup gives a store, a broker, a fake remote and a live attempt r_1 at fence 1.
func setup(t *testing.T) (context.Context, *store.Store, *Broker, *Fake) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	st, err := store.Open(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Truncate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, "TRUNCATE effects"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMission(ctx, "m_1", "GoodQA goodqa#14791", "fabric", "main"); err != nil {
		t.Fatal(err)
	}
	must := func(key string, cmd domain.Command) {
		if _, err := st.Execute(ctx, "fabd", domain.CommandID(key), cmd); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	must("admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	must("start", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_1", Attempt: "r_1", Host: "h_t2", Role: "writer"})
	return ctx, st, New(st), NewFake()
}

func spec(key, body string) IntentSpec {
	return IntentSpec{Mission: "m_1", Attempt: "r_1", Fence: 1, Kind: "ticket_comment", Subject: "ticket:goodqa#0",
		IdemKey: key, Request: json.RawMessage(`{"ticket": "goodqa#0", "body": "` + body + `"}`)}
}

func TestHappyPathOneRemoteWrite(t *testing.T) {
	ctx, _, b, fake := setup(t)
	e, err := b.Submit(ctx, fake, spec("k1", "hello"))
	if err != nil || e.State != Intent || e.Replayed {
		t.Fatalf("submit: %+v %v", e, err)
	}
	e, err = b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || e.Dispatches != 1 || fake.Count() != 1 {
		t.Fatalf("dispatch: %+v %v remote=%d", e, err, fake.Count())
	}
	// Re-submitting the same key and body replays; re-dispatching a CONFIRMED effect is refused.
	again, err := b.Submit(ctx, fake, spec("k1", "hello"))
	if err != nil || !again.Replayed || again.ID != e.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("second dispatch: want ErrNotDispatchable, got %v", err)
	}
	if fake.Count() != 1 {
		t.Fatalf("remote writes = %d, want 1", fake.Count())
	}
}

// N2: the response is lost after the remote applied the write. The effect
// is UNKNOWN, a retry is refused, readback finds the write, one remote entry.
func TestPostCommitLossReconcilesToConfirmed(t *testing.T) {
	ctx, _, b, fake := setup(t)
	fake.LoseAfter = true
	e, _ := b.Submit(ctx, fake, spec("k2", "once"))
	e, err := b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Unknown {
		t.Fatalf("want UNKNOWN, got %s (%v)", e.State, err)
	}
	if _, err := b.Retry(ctx, e.ID); !errors.Is(err, domain.ErrUnknownUnresolved) {
		t.Fatalf("retry during UNKNOWN: want ErrUnknownUnresolved, got %v", err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("dispatch during UNKNOWN: want ErrNotDispatchable, got %v", err)
	}
	fake.LoseAfter = false
	e, err = b.Reconcile(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("reconcile: %s %v remote=%d", e.State, err, fake.Count())
	}
}

// N2: the request is lost before the remote saw it. Readback proves absence,
// the same key is retried, one remote entry.
func TestPreCommitLossRetriesSameKey(t *testing.T) {
	ctx, _, b, fake := setup(t)
	fake.LoseBefor = true
	e, _ := b.Submit(ctx, fake, spec("k3", "once"))
	e, _ = b.Dispatch(ctx, fake, e.ID)
	if e.State != Unknown {
		t.Fatalf("want UNKNOWN, got %s", e.State)
	}
	fake.LoseBefor = false
	e, err := b.Reconcile(ctx, fake, e.ID)
	if err != nil || e.State != NotApplied {
		t.Fatalf("reconcile: %s %v", e.State, err)
	}
	e, err = b.Retry(ctx, e.ID)
	if err != nil || e.State != Intent || e.IdemKey != "k3" {
		t.Fatalf("retry: %+v %v", e, err)
	}
	e, err = b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || e.Dispatches != 2 || fake.Count() != 1 {
		t.Fatalf("second dispatch: %s %v dispatches=%d remote=%d", e.State, err, e.Dispatches, fake.Count())
	}
}

func TestSameKeyDifferentBodyConflicts(t *testing.T) {
	ctx, _, b, fake := setup(t)
	if _, err := b.Submit(ctx, fake, spec("k4", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(ctx, fake, spec("k4", "b")); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
	reordered := spec("k4", "a")
	reordered.Request = json.RawMessage(`{"body":"a","ticket":"goodqa#0"}`)
	if e, err := b.Submit(ctx, fake, reordered); err != nil || !e.Replayed {
		t.Fatalf("reordered keys should replay: %+v %v", e, err)
	}
}

func TestStaleFenceAndSubjectBusy(t *testing.T) {
	ctx, _, b, fake := setup(t)
	s := spec("k5", "x")
	s.Fence = 0
	if _, err := b.Submit(ctx, fake, s); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale fence: want ErrStaleFence, got %v", err)
	}
	// Two effects on one subject: the second cannot dispatch while the first is UNKNOWN.
	fake.LoseAfter = true
	e1, _ := b.Submit(ctx, fake, spec("k6", "first"))
	e1, _ = b.Dispatch(ctx, fake, e1.ID)
	if e1.State != Unknown {
		t.Fatalf("want UNKNOWN, got %s", e1.State)
	}
	e2, _ := b.Submit(ctx, fake, spec("k7", "second"))
	if _, err := b.Dispatch(ctx, fake, e2.ID); !errors.Is(err, ErrSubjectBusy) {
		t.Fatalf("subject busy: want ErrSubjectBusy, got %v", err)
	}
	if fake.Count() != 1 {
		t.Fatalf("remote writes = %d, want 1 (the second must not have gone out)", fake.Count())
	}
}

// AC-17: an approval binds the request hash; a wrong hash is refused and an
// unapproved effect cannot dispatch.
func TestApprovalBindsRequestHash(t *testing.T) {
	ctx, _, b, fake := setup(t)
	s := spec("k8", "publish")
	s.NeedsApproval = true
	e, err := b.Submit(ctx, fake, s)
	if err != nil || e.State != ApprovalRequired {
		t.Fatalf("submit: %+v %v", e, err)
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("dispatch without approval: want ErrApprovalRequired, got %v", err)
	}
	other, _ := domain.EffectBase("ticket_comment", json.RawMessage(`{"ticket":"goodqa#0","body":"tampered"}`))
	if err := b.Approve(ctx, e.ID, other, "user"); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("approval with another hash: want ErrApprovalMismatch, got %v", err)
	}
	if err := b.Approve(ctx, e.ID, e.RequestSHA, "user"); err != nil {
		t.Fatal(err)
	}
	e, err = b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("approved dispatch: %s %v remote=%d", e.State, err, fake.Count())
	}
}

// Crash cutpoints inside the broker itself: a crash after the DISPATCHED row
// commits but before transport leaves UNKNOWN-shaped state that only a
// reconcile resolves; a crash after transport but before the receipt commits
// is the same. Neither path dispatches twice.
func TestBrokerCutpoints(t *testing.T) {
	ctx, _, b, fake := setup(t)
	cut := errors.New("process died")
	b.afterDispatchCommit = func(Effect) { panic(cut) }
	e, _ := b.Submit(ctx, fake, spec("k9", "cut-before-transport"))
	func() {
		defer func() { recover() }() //nolint:errcheck
		b.Dispatch(ctx, fake, e.ID)  //nolint:errcheck
	}()
	b.afterDispatchCommit = nil
	e, _ = b.Load(ctx, e.ID)
	if e.State != Dispatched || fake.Count() != 0 {
		t.Fatalf("after the cut: state=%s remote=%d", e.State, fake.Count())
	}
	if _, err := b.Dispatch(ctx, fake, e.ID); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("a DISPATCHED row must not dispatch again: %v", err)
	}
	// Recovery: the sweep marks stale DISPATCHED rows UNKNOWN, then reconciles.
	if _, err := b.pool.Exec(ctx, "UPDATE effects SET state = $2 WHERE id = $1 AND state = $3", e.ID, string(Unknown), string(Dispatched)); err != nil {
		t.Fatal(err)
	}
	e, err := b.Reconcile(ctx, fake, e.ID)
	if err != nil || e.State != NotApplied {
		t.Fatalf("reconcile after a pre-transport crash: %s %v", e.State, err)
	}
	e, _ = b.Retry(ctx, e.ID)
	e, err = b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("after retry: %s %v remote=%d", e.State, err, fake.Count())
	}

	// Cut after transport, before the receipt commits.
	b.afterTransport = func(Effect, Receipt) { panic(cut) }
	e2, _ := b.Submit(ctx, fake, spec("k10", "cut-after-transport"))
	func() {
		defer func() { recover() }() //nolint:errcheck
		b.Dispatch(ctx, fake, e2.ID) //nolint:errcheck
	}()
	b.afterTransport = nil
	e2, _ = b.Load(ctx, e2.ID)
	if e2.State != Dispatched || fake.Count() != 2 {
		t.Fatalf("after the cut: state=%s remote=%d", e2.State, fake.Count())
	}
	b.pool.Exec(ctx, "UPDATE effects SET state = $2 WHERE id = $1", e2.ID, string(Unknown)) //nolint:errcheck
	e2, err = b.Reconcile(ctx, fake, e2.ID)
	if err != nil || e2.State != Confirmed || fake.Count() != 2 {
		t.Fatalf("reconcile after a post-transport crash: %s %v remote=%d", e2.State, err, fake.Count())
	}
}

func TestDefiniteRejectRetriesWithSameKey(t *testing.T) {
	ctx, _, b, fake := setup(t)
	fake.Reject = true
	e, _ := b.Submit(ctx, fake, spec("k11", "x"))
	e, _ = b.Dispatch(ctx, fake, e.ID)
	if e.State != FailedDefinite || fake.Count() != 0 {
		t.Fatalf("want FAILED_DEFINITE with no write, got %s remote=%d", e.State, fake.Count())
	}
	fake.Reject = false
	e, _ = b.Retry(ctx, e.ID)
	e, err := b.Dispatch(ctx, fake, e.ID)
	if err != nil || e.State != Confirmed || e.IdemKey != "k11" || fake.Count() != 1 {
		t.Fatalf("retry: %s %v key=%s remote=%d", e.State, err, e.IdemKey, fake.Count())
	}
}
