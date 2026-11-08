package sched

import (
	"context"
	"errors"
	"testing"
	"time"
)

// recordSleep records requested sleeps without sleeping: no wall-clock timing.
type recordSleep struct{ got []time.Duration }

func (r *recordSleep) sleep(_ context.Context, d time.Duration) error {
	r.got = append(r.got, d)
	return nil
}

// A broken LISTEN connection fails at once while the wait context is live.
// Without a sleep, Run would call Tick again immediately: a hot loop.
func TestListenErrorBacksOffWithoutHotSpin(t *testing.T) {
	broken := errors.New("conn closed")
	for _, werr := range []error{broken, context.DeadlineExceeded} {
		var rs recordSleep
		notified, err := waitForWake(context.Background(), func(context.Context) error { return werr }, 250*time.Millisecond, rs.sleep)
		if err != nil || notified {
			t.Fatalf("%v: notified=%v err=%v, want a quiet miss", werr, notified, err)
		}
		if len(rs.got) != 1 || rs.got[0] != 250*time.Millisecond {
			t.Fatalf("%v returned immediately: sleeps %v, want one sleep of the backoff", werr, rs.got)
		}
	}
}

// A wait whose own deadline expired already spent the backoff: no extra sleep.
// A notification needs none either.
func TestExpiredWaitAndNotificationNeedNoExtraSleep(t *testing.T) {
	var rs recordSleep
	expire := func(wctx context.Context) error { <-wctx.Done(); return wctx.Err() }
	notified, err := waitForWake(context.Background(), expire, time.Millisecond, rs.sleep)
	if err != nil || notified || len(rs.got) != 0 {
		t.Fatalf("expired wait: notified=%v err=%v sleeps %v", notified, err, rs.got)
	}
	notified, err = waitForWake(context.Background(), func(context.Context) error { return nil }, time.Hour, rs.sleep)
	if err != nil || !notified || len(rs.got) != 0 {
		t.Fatalf("notification: notified=%v err=%v sleeps %v", notified, err, rs.got)
	}
}

func TestWaitForWakeStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var rs recordSleep
	if _, err := waitForWake(ctx, func(c context.Context) error { return c.Err() }, time.Hour, rs.sleep); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}

func TestNextBackoffDoublesAndClamps(t *testing.T) {
	max := 30 * time.Second
	cur := 500 * time.Millisecond
	var seen []time.Duration
	for i := 0; i < 10; i++ {
		cur = nextBackoff(cur, max)
		seen = append(seen, cur)
		if cur > max {
			t.Fatalf("backoff %v passed max %v (sequence %v)", cur, max, seen)
		}
	}
	if seen[0] != time.Second || cur != max {
		t.Fatalf("sequence %v, want doubling from 1s to the 30s cap", seen)
	}
}
