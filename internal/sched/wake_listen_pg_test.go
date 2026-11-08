package sched

import (
	"context"
	"os"
	"testing"
	"time"

	"fabric/internal/store"
)

// goodqa#15299 AC-4: the real-Postgres LISTEN path under pgx v5.9.2. Run
// subscribes with "LISTEN "+WakeChannel and blocks in WaitForNotification
// through waitForWake (sched.go Run); writers call Notify after a commit. A
// NOTIFY must arrive as a wake well inside a backoff that would otherwise
// hide its loss. Changing the channel name on either side makes this fail.
func TestWakeNotifyReachesListenerOverPostgres(t *testing.T) {
	dsn := os.Getenv("FABRIC_TEST_DSN")
	if dsn == "" {
		t.Skip("set FABRIC_TEST_DSN (a disposable PostgreSQL) to run the LISTEN test")
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
