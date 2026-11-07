package settle_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
	"fabric/internal/settle"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "set")) }

// fakeClock moves only when Finish waits on it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	t := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- t
	return ch
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)} }

// liveAttempt gives a RUNNING claude-code attempt to finish.
func liveAttempt(t *testing.T, st *store.Store, m domain.MissionID, a domain.AttemptID) int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateMission(ctx, m, domain.ProviderClaudeCode, "GoodQA goodqa#15087", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx, "chat", domain.CommandID("admit-"+string(m)), domain.Command{Kind: domain.CmdAdmitMission, Mission: m}); err != nil {
		t.Fatal(err)
	}
	resp, err := st.Execute(ctx, "fabd", domain.CommandID("start:"+string(a)), domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a,
		Host: "h_t2", Role: "writer", Path: domain.ProviderClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	fence := resp.Events[0].Fence
	if _, err := st.Execute(ctx, "fabd", domain.CommandID("ack:"+string(a)), domain.Command{Kind: domain.CmdAckStart, Mission: m, Attempt: a, Fence: fence}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func finishCounts(t *testing.T, st *store.Store, m domain.MissionID, a domain.AttemptID) (cmds, events int) {
	t.Helper()
	if err := st.Pool().QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM commands WHERE mission_id = $1 AND command_id = $2),
		(SELECT count(*) FROM events WHERE mission_id = $1 AND kind = 'ATTEMPT_FINISHED')`, string(m), "finish:"+string(a)).Scan(&cmds, &events); err != nil {
		t.Fatal(err)
	}
	return
}

var lostReply = &pgconn.PgError{Code: "08006", Message: "test: connection lost after commit"}

// AC-7: fault 1 (an error before commit) and fault 2 (the finish commits,
// then the caller sees an error) are retried and end in exactly one finish;
// fault 3 (a domain refusal) is returned after one call.
func TestFinishRetries(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("fault%d", fault), func(t *testing.T) {
			st := storetest.Open(t)
			m := domain.MissionID(fmt.Sprintf("m_f%d", fault))
			a := domain.AttemptID(fmt.Sprintf("r_f%d", fault))
			fence := liveAttempt(t, st, m, a)
			cmd := domain.Command{Kind: domain.CmdFinishAttempt, Mission: m, Attempt: a, Fence: fence, Outcome: "checkpoint"}
			if fault == 3 {
				cmd.Fence = fence + 7 // stale: the reducer refuses it
			}
			calls := 0
			var replayed []bool
			err := settle.Finish(ctx, settle.Options{Clock: newClock()}, func(ctx context.Context) error {
				calls++
				if fault == 1 && calls == 1 {
					return lostReply // nothing reached the database
				}
				resp, err := st.Execute(ctx, "fabd", domain.CommandID("finish:"+string(a)), cmd)
				replayed = append(replayed, resp.Replayed)
				if fault == 2 && calls == 1 && err == nil {
					return lostReply // committed, but the reply is lost
				}
				return err
			})
			cmds, events := finishCounts(t, st, m, a)
			switch fault {
			case 1, 2:
				if err != nil || calls != 2 || cmds != 1 || events != 1 {
					t.Fatalf("fault %d: err %v, calls %d, command rows %d, finish events %d; want nil, 2, 1, 1", fault, err, calls, cmds, events)
				}
				if fault == 2 && (len(replayed) != 2 || replayed[0] || !replayed[1]) {
					t.Fatalf("fault 2: replayed flags %v, want [false true]", replayed)
				}
			case 3:
				if !errors.As(err, new(*store.Refusal)) || calls != 1 || events != 0 {
					t.Fatalf("fault 3: err %v, calls %d, finish events %d; want a refusal after 1 call and no finish", err, calls, events)
				}
			}
		})
	}
}

// AC-7: a store that always fails transiently is called at most 6 times and
// for at most 60 s of fake clock; Finish then returns that error.
func TestFinishGivesUp(t *testing.T) {
	for _, c := range []struct {
		name    string
		backoff time.Duration
	}{{"tries bound", time.Millisecond}, {"time bound", 20 * time.Second}} {
		t.Run(c.name, func(t *testing.T) {
			clock := newClock()
			start := clock.Now()
			calls := 0
			err := settle.Finish(context.Background(), settle.Options{Clock: clock, Backoff: c.backoff}, func(context.Context) error {
				calls++
				return lostReply
			})
			elapsed := clock.Now().Sub(start)
			if !errors.Is(err, lostReply) || calls < 2 || calls > settle.MaxTries || elapsed > settle.Budget {
				t.Fatalf("err %v, %d calls over %s; want the transient error, 2..%d calls, at most %s", err, calls, elapsed, settle.MaxTries, settle.Budget)
			}
			t.Logf("%d calls over %s of fake clock", calls, elapsed)
		})
	}
}

// Only database errors that can succeed on a retry are transient.
func TestTransient(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "40001"}, true},
		{&pgconn.PgError{Code: "40P01"}, true},
		{&pgconn.PgError{Code: "08006"}, true},
		{&pgconn.PgError{Code: "57P01"}, true},
		{fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), true},
		{&pgconn.PgError{Code: "P0001"}, false}, // a trigger RAISE
		{&pgconn.PgError{Code: "23514"}, false},
		{&store.Refusal{Err: domain.ErrInvalidTransition}, false},
		{errors.New("unknown"), false},
		{context.Canceled, false},
	} {
		if got := settle.Transient(c.err); got != c.want {
			t.Errorf("Transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
