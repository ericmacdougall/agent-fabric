package store

// Failing-first tests ported from the 8f83662f review reproducers (RV-2,
// RV-3, RV-8, RV-21, RV-22). They failed against 8f83662f and pass with the
// fixes.

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fabric/internal/domain"
)

// RV-3 (H5): a row whose transaction commits later with a lower id is still
// delivered; the consumer waits for it instead of acking past it.
func TestRV3_OutboxDeliversLateCommit(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_a")
	mk(t, st, ctx, "m_b")
	inHook := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failing assertion must not leave the hook blocked (the pool close would hang)
	st.hooks.beforeCommit = func(cmd domain.Command) error {
		if cmd.Mission == "m_a" {
			close(inHook)
			<-release
		}
		return nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st.Execute(ctx, "host", "wa", domain.Command{Kind: domain.CmdWake, Mission: "m_a", Token: "job_a"}) //nolint:errcheck
	}()
	<-inHook // m_a holds the lower outbox id, uncommitted
	ok(t, st, ctx, "host", "wb", domain.Command{Kind: domain.CmdWake, Mission: "m_b", Token: "job_b"})
	page1, err := st.OutboxPage(ctx, "poster", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page1 {
		if r.Mission == "m_b" {
			t.Fatal("the later-committing lower id can still be skipped: m_b was delivered before m_a committed")
		}
	}
	unblock()
	wg.Wait()
	st.hooks.beforeCommit = nil
	// Visibility waits for every older in-progress transaction in the
	// cluster (pg_snapshot_xmin is cluster-wide), so a consumer sees both
	// rows on a later page, in id order, never m_b alone.
	var page2 []OutboxRow
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		page2, _ = st.OutboxPage(ctx, "poster", 10)
		if len(page2) == 2 {
			break
		}
		if len(page2) == 1 && page2[0].Mission == "m_b" {
			t.Fatalf("m_b delivered without m_a: %+v", page2)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(page2) != 2 || page2[0].Mission != "m_a" || page2[1].Mission != "m_b" {
		t.Fatalf("after both commits: %+v", page2)
	}
}

// RV-2 (H6): with one live attempt per role, writer and reviewer both expire
// and a limit-1 sweep reaches each in turn; nothing starves.
func TestRV2_SweepReachesEveryExpiredLease(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 100 * time.Millisecond
	mk(t, st, ctx, "m_1")
	mk(t, st, ctx, "m_2")
	ok(t, st, ctx, "chat", "a1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	ok(t, st, ctx, "chat", "a2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"})
	startAttempt(t, st, ctx, "m_1", "r_1")
	ok(t, st, ctx, "fabd", "start:r_rev", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_1", Attempt: "r_rev", Host: "h_t2", Role: "reviewer"})
	startAttempt(t, st, ctx, "m_2", "r_3")
	time.Sleep(250 * time.Millisecond)
	total := 0
	for i := 0; i < 3; i++ {
		exp, err := st.ExpireLeases(ctx, 1)
		if err != nil || len(exp) != 1 {
			t.Fatalf("sweep %d: %v %v", i, exp, err)
		}
		total += len(exp)
	}
	if total != 3 {
		t.Fatalf("expired %d of 3", total)
	}
}

// RV-21 (H10): a long but valid attempt id does not poison the sweep, and an
// invalid attempt id is refused at the door.
func TestRV21_LongAttemptIDDoesNotStallSweep(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 50 * time.Millisecond
	mk(t, st, ctx, "m_1")
	mk(t, st, ctx, "m_2")
	ok(t, st, ctx, "chat", "a1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	ok(t, st, ctx, "chat", "a2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_2"})
	long := domain.AttemptID("r_" + strings.Repeat("a", 60))
	ok(t, st, ctx, "fabd", "s1", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_1", Attempt: long, Host: "h_t2", Role: "writer"})
	ok(t, st, ctx, "fabd", "s2", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_2", Attempt: "r_ok", Host: "h_t2", Role: "writer"})
	if _, err := st.Execute(ctx, "fabd", "s3", domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_2", Attempt: "not an id/../x", Host: "h_t2"}); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("invalid attempt id: want ErrInvalidID, got %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	exp, err := st.ExpireLeases(ctx, 10)
	if err != nil || len(exp) != 2 {
		t.Fatalf("sweep: %v %v", exp, err)
	}
}

// RV-22 (H10): a lease renewed after the sweep read it is not expired.
func TestRV22_RenewedLeaseNotExpired(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 100 * time.Millisecond
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	time.Sleep(150 * time.Millisecond)
	if err := st.Heartbeat(ctx, "r_1", fence); err != nil {
		t.Fatal(err)
	}
	_, err := st.Execute(ctx, "fabd", "expire:f1:stale-read", domain.Command{Kind: domain.CmdExpireLease, Mission: "m_1", Attempt: "r_1", Fence: fence})
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("expiry of a renewed lease: want a refusal, got %v", err)
	}
	m, _ := st.Mission(ctx, "m_1")
	if m.Attempts["r_1"].State == domain.AttemptUnknown {
		t.Fatal("renewed lease was expired")
	}
}

// RV-8: an oversized command (any field) is refused at the door and nothing
// is stored, even under a stale fence.
func TestRV8_OversizedCommandRefused(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	_, err := st.Execute(ctx, "host", "big-reason", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_1", Attempt: "r_1", Fence: fence - 1, Outcome: "checkpoint", Reason: strings.Repeat("x", MaxCommandBytes+1)})
	if err == nil || errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("oversized reason: want a size refusal before the fence check, got %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM stale_observations") + count(t, st, ctx, "SELECT count(*) FROM commands WHERE command_id = 'big-reason'"); n != 0 {
		t.Fatalf("oversized command left %d rows", n)
	}
}

// S5 (Kimi P3 reconciliation): a renewed-lease refusal is stored like any
// response, so the same key later replays it instead of deciding again.
func TestRenewedLeaseRefusalReplays(t *testing.T) {
	st, ctx := openTest(t)
	st.Lease = 100 * time.Millisecond
	mk(t, st, ctx, "m_1")
	ok(t, st, ctx, "chat", "admit", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_1"})
	fence := startAttempt(t, st, ctx, "m_1", "r_1")
	time.Sleep(150 * time.Millisecond)
	if err := st.Heartbeat(ctx, "r_1", fence); err != nil {
		t.Fatal(err)
	}
	cmd := domain.Command{Kind: domain.CmdExpireLease, Mission: "m_1", Attempt: "r_1", Fence: fence}
	_, err := st.Execute(ctx, "fabd", "expire:f1:observed", cmd)
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("expiry of a renewed lease: want a refusal, got %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM commands WHERE command_id = 'expire:f1:observed'"); n != 1 {
		t.Fatalf("the refusal was not stored (%d rows)", n)
	}
	time.Sleep(150 * time.Millisecond) // the renewed lease lapses too
	resp, err := st.Execute(ctx, "fabd", "expire:f1:observed", cmd)
	if !errors.As(err, &ref) || !resp.Replayed {
		t.Fatalf("same key after the lease lapsed: want the stored refusal replayed, got %+v %v", resp, err)
	}
	if m, _ := st.Mission(ctx, "m_1"); m.Attempts["r_1"].State == domain.AttemptUnknown {
		t.Fatal("a replayed key expired the attempt")
	}
}
