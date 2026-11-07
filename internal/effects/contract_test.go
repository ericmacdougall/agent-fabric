package effects

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AssertHonoursDeadline is the Executor deadline contract (review W2 on
// cbebbd6): every call returns by its context deadline (plus a small grace)
// against a remote that never answers. Use it for every executor.
func assertHonoursDeadline(t *testing.T, name string, ex Executor, e Effect) {
	t.Helper()
	const limit, grace = 150 * time.Millisecond, time.Second
	// Each call returns a contract violation, or nil. A hung Precondition
	// must fail; a hung Dispatch or Reconcile must fail or report UNKNOWN.
	calls := map[string]func(context.Context) error{
		"Precondition": func(ctx context.Context) error {
			if ok, _, err := ex.Precondition(ctx, e); err == nil {
				return fmt.Errorf("hung precondition returned ok=%v and no error", ok)
			}
			return nil
		},
		"Dispatch": func(ctx context.Context) error {
			if rc, err := ex.Dispatch(ctx, Submission{Effect: e, RemoteKey: e.IdemKey}); err == nil && rc.Outcome != OutcomeUnknown {
				return errors.New("hung dispatch reported " + string(rc.Outcome))
			}
			return nil
		},
		"Reconcile": func(ctx context.Context) error {
			if obs, _, err := ex.Reconcile(ctx, e); err == nil && obs != ObsUnknown {
				return errors.New("hung readback reported " + string(obs))
			}
			return nil
		},
	}
	for call, f := range calls {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		start := time.Now()
		err := f(ctx)
		cancel()
		if took := time.Since(start); took > limit+grace {
			t.Errorf("%s.%s returned after %s, past its %s deadline", name, call, took, limit)
		}
		if err != nil {
			t.Errorf("%s.%s: %v", name, call, err)
		}
	}
}

func TestExecutorsHonourDeadline(t *testing.T) {
	e := Effect{ID: "e_c", Kind: "ticket_comment", IdemKey: "contract", Request: []byte(`{"issue_id":"0c0223e2-a47b-4bb4-83ca-d7794142ee5c","ticket":"goodqa#1","body":"x"}`)}
	f := NewFake()
	f.Hang = true
	assertHonoursDeadline(t, "Fake", f, e)
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hung:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hung); srv.Close() })
	keyFile := filepath.Join(t.TempDir(), "member.key")
	os.WriteFile(keyFile, []byte("test-key\n"), 0o600) //nolint:errcheck
	assertHonoursDeadline(t, "GoodQA", &GoodQA{Endpoint: srv.URL, KeyFile: keyFile}, e)
}

// Review W2: an executor that overruns its deadline is counted, so a
// contract breach is visible.
func TestExecutorOverrunCounted(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	b.SetTransportTimeout(100 * time.Millisecond)
	e, err := b.Submit(ctx, fake, uspec("overrun"))
	if err != nil {
		t.Fatal(err)
	}
	fake.Stall = 600 * time.Millisecond
	if _, err := b.Dispatch(ctx, fake, e.ID); err != nil {
		t.Fatal(err)
	}
	if n := b.ExecutorOverruns(); n < 1 {
		t.Fatalf("executor overruns %d, want at least 1", n)
	}
}

// Review nit 2 on cbebbd6: a settle whose commit landed but whose reply was
// lost is recognised on retry, not reported as lost.
func TestSettleRecognisesItsOwnCommit(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	e, err := b.Submit(ctx, fake, uspec("lostreply"))
	if err != nil {
		t.Fatal(err)
	}
	b.afterTransport = func(Effect, Receipt) { panic("cut") }
	func() {
		defer func() { recover() }() //nolint:errcheck
		b.Dispatch(ctx, fake, e.ID)  //nolint:errcheck
	}()
	b.afterTransport = nil
	c, err := b.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	rc := Receipt{Outcome: OutcomeConfirmed}
	if err := b.settle(ctx, c, e.ID, 1, rc); err != nil {
		t.Fatal(err)
	}
	if err := b.settle(ctx, c, e.ID, 1, rc); err != nil {
		t.Errorf("a repeated settle of the committed outcome: %v, want nil", err)
	}
	if n := b.SettleLost(); n != 0 {
		t.Errorf("settle lost %d, want 0", n)
	}
	if err := b.settle(ctx, c, e.ID, 1, Receipt{Outcome: OutcomeFailedDefinite}); !errors.Is(err, ErrSettleLost) {
		t.Errorf("a contradicting settle: %v, want ErrSettleLost", err)
	}
}

// Review nit 1: the resettle closes the dead connection before taking a
// fresh one, so an otherwise full pool does not hang it.
func TestResettleWithFullPool(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	e, err := b.Submit(ctx, fake, uspec("fullpool"))
	if err != nil {
		t.Fatal(err)
	}
	ps := parkedSend{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := b.Dispatch(ctx, ps, e.ID); done <- err }()
	<-ps.entered
	var killed bool
	if err := st.Pool().QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory'
		AND classid = 4011 AND objid = hashtext($1)::oid AND objsubid = 2`, e.ID).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the dispatcher session: %v %v", killed, err)
	}
	var held []interface{ Release() }
	defer func() {
		for _, h := range held {
			h.Release()
		}
	}()
	for i := 1; i < int(st.Pool().Config().MaxConns); i++ {
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := st.Pool().Acquire(actx)
		cancel()
		if err != nil {
			t.Fatalf("fill the pool (%d): %v", i, err)
		}
		held = append(held, c)
	}
	close(ps.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	case <-time.After(15 * time.Second):
		for _, h := range held {
			h.Release()
		}
		held = nil
		<-done
		t.Fatal("resettle hung on a full pool")
	}
	for _, h := range held {
		h.Release()
	}
	held = nil
	if got, _ := b.Load(ctx, e.ID); got.State != Confirmed {
		t.Fatalf("after resettle: %s, want CONFIRMED", got.State)
	}
}

// Review nit 3: the dispatch cap belongs to the store's pool, shared by
// every broker on it.
func TestDispatchCapSharedAcrossBrokers(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	b2 := New(st)
	n := b.DispatchLimit()
	ss := slowSend{Fake: fake, entered: make(chan string, n+1), release: make(chan struct{})}
	done := make(chan error, n+1)
	for i := 0; i <= n; i++ {
		e, err := b.Submit(ctx, fake, uspec("cap"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		br := b
		if i%2 == 1 {
			br = b2
		}
		go func(br *Broker, id string) { _, err := br.Dispatch(ctx, ss, id); done <- err }(br, e.ID)
	}
	for i := 0; i < n; i++ {
		<-ss.entered
	}
	select {
	case id := <-ss.entered:
		t.Errorf("dispatch %s entered beyond the shared limit %d", id, n)
	case <-time.After(300 * time.Millisecond):
	}
	close(ss.release)
	for i := 0; i <= n; i++ {
		<-done
	}
}

// Review nit 4: staleness follows the deadline stamped when the effect was
// dispatched, not the reconciler's own timeout setting.
func TestStaleFollowsStampedDeadline(t *testing.T) {
	ctx, st, b, fake := setup(t)
	ackRun(t, ctx, st)
	runnerBroker(t, ctx, st)
	ab := appBroker(t, ctx, st)
	e, err := b.Submit(ctx, fake, uspec("stamped"))
	if err != nil {
		t.Fatal(err)
	}
	ps := parkedSend{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := b.Dispatch(ctx, ps, e.ID); done <- err }()
	<-ps.entered
	var killed bool
	if err := st.Pool().QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory'
		AND classid = 4011 AND objid = hashtext($1)::oid AND objsubid = 2`, e.ID).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the dispatcher session: %v %v", killed, err)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE attempts SET state = 'UNKNOWN' WHERE id = 'r_1'"); err != nil {
		t.Fatal(err)
	}
	// A reconciler configured with a much shorter timeout.
	ab.SetTransportTimeout(50 * time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if ids, _ := ab.Unresolved(ctx, 0, 10); len(ids) != 0 {
		t.Errorf("unresolved ignores the stamped deadline: %v", ids)
	}
	if _, err := ab.Reconcile(ctx, fake, e.ID); !errors.Is(err, ErrNotReconcilable) {
		t.Errorf("reconcile before the stamped deadline: %v, want ErrNotReconcilable", err)
	}
	close(ps.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := ab.Load(ctx, e.ID); got.State != Confirmed || fake.Count() != 1 {
		t.Fatalf("after the late transport: %s remote=%d", got.State, fake.Count())
	}
}
