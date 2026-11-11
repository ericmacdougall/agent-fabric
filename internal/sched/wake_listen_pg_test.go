package sched

import (
	"context"
	"os"
	"testing"
	"time"

	"fabric/internal/connref"
	"fabric/internal/store"
)

// goodqa#15299 AC-4: the real-Postgres LISTEN path under pgx v5.9.2. Run
// subscribes with "LISTEN "+WakeChannel and blocks in WaitForNotification
// through waitForWake (sched.go Run); writers call Notify after a commit. A
// NOTIFY must arrive as a wake well inside a backoff that would otherwise
// hide its loss. Changing the channel name on either side makes this fail.
func TestWakeNotifyReachesListenerOverPostgres(t *testing.T) {
	if os.Getenv("FABRIC_TEST_DSN") == "" && !connref.HasReferenceConfig("FABRIC_TEST_DSN") {
		t.Skip("set FABRIC_TEST_DSN or FABRIC_TEST_DSN_FILE (a disposable PostgreSQL) to run the LISTEN test")
	}
	ref, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		t.Fatal("test credential reference unavailable")
	}
	defer ref.Close()
	dsn, err := ref.DSN()
	if err != nil {
		t.Fatal("test credential reference unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+WakeChannel); err != nil {
		t.Fatal(err)
	}
	Notify(ctx, st)
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	start := time.Now()
	notified, err := waitForWake(ctx, func(wctx context.Context) error {
		_, err := conn.Conn().WaitForNotification(wctx)
		return err
	}, 5*time.Second, sleep)
	if err != nil || !notified || len(slept) != 0 {
		t.Fatalf("NOTIFY %s did not wake the listener: notified=%v err=%v sleeps=%v after %v", WakeChannel, notified, err, slept, time.Since(start))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("wake took %v, want well under the 5 s backoff", d)
	}
}

// goodqa#15299 AC-4 (SX-6): Run's idle path. With nothing to do, each
// waitForWake expires at its deadline and Run reuses the same LISTEN
// connection for the next wait. Under pgx v5.9.2 an expired
// WaitForNotification must leave that connection open and listening: each
// expiry returns (false, nil) without the error-path sleep, and a NOTIFY sent
// after two expiries still wakes the same connection. A pgx that closed or
// desynchronised the connection on a deadline would make every later wait
// fail at once ("fabd: LISTEN wait failed") and sleep, so Run would quietly
// fall back to polling; that shows here as a recorded sleep.
func TestWakeListenerSurvivesIdleDeadlines(t *testing.T) {
	if os.Getenv("FABRIC_TEST_DSN") == "" && !connref.HasReferenceConfig("FABRIC_TEST_DSN") {
		t.Skip("set FABRIC_TEST_DSN or FABRIC_TEST_DSN_FILE (a disposable PostgreSQL) to run the LISTEN test")
	}
	ref, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		t.Fatal("test credential reference unavailable")
	}
	defer ref.Close()
	dsn, err := ref.DSN()
	if err != nil {
		t.Fatal("test credential reference unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+WakeChannel); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	wait := func(wctx context.Context) error {
		_, err := conn.Conn().WaitForNotification(wctx) // idle-wait
		return err
	}
	const idle = 200 * time.Millisecond
	for i := 1; i <= 2; i++ {
		start := time.Now()
		notified, err := waitForWake(ctx, wait, idle, sleep)
		d := time.Since(start)
		if err != nil || notified || len(slept) != 0 {
			t.Fatalf("idle wait %d: notified=%v err=%v sleeps=%v after %v; want an expiry (false, nil) with no error-path sleep", i, notified, err, slept, d)
		}
		if d < idle || d > 3*time.Second {
			t.Fatalf("idle wait %d took %v, want its %v deadline (an early return means the wait failed instead of expiring)", i, d, idle)
		}
	}
	if conn.Conn().IsClosed() {
		t.Fatal("the LISTEN connection was closed by an expired wait")
	}
	Notify(ctx, st) // after-idle NOTIFY
	start := time.Now()
	notified, err := waitForWake(ctx, wait, 5*time.Second, sleep)
	if err != nil || !notified || len(slept) != 0 {
		t.Fatalf("NOTIFY %s after two expired waits did not wake the same connection: notified=%v err=%v sleeps=%v after %v", WakeChannel, notified, err, slept, time.Since(start))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("wake after idle expiries took %v, want well under the 5 s backoff", d)
	}
}
