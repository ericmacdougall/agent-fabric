package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	"fabric/internal/domain"
)

// Red-team N3 on 42c7bd2: the pre-launch flags change the park budget and
// the backoff, so only the scheduler may set them. Two layers: Execute
// refuses them from any actor but fabd, and 0015 refuses the events they
// produce from any database role without the privileges of the schema
// owner, which is what a runner's own role lacks.

func TestPrelaunchFlagsReservedToFabd(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_pr1")
	ok(t, st, ctx, "chat", "admit-pr1", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_pr1"})
	fence := startAttempt(t, st, ctx, "m_pr1", "r_pr1_1")
	for _, c := range []domain.Command{
		{Kind: domain.CmdFinishAttempt, Mission: "m_pr1", Attempt: "r_pr1_1", Fence: fence, Outcome: "abandoned", Prelaunch: true},
		{Kind: domain.CmdFinishAttempt, Mission: "m_pr1", Attempt: "r_pr1_1", Fence: fence, Outcome: "abandoned", Prelaunch: true, CapacityWait: true},
	} {
		if _, err := st.Execute(ctx, "runner", "fin-runner-pr1", c); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("a runner set the pre-launch flags: %v", err)
		}
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM events WHERE mission_id = 'm_pr1' AND kind = 'DISPATCH_DEFERRED'"); n != 0 {
		t.Fatalf("%d deferrals written for a refused runner", n)
	}
	ok(t, st, ctx, "fabd", "fin-fabd-pr1", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_pr1", Attempt: "r_pr1_1",
		Fence: fence, Outcome: "abandoned", Prelaunch: true})
}

func TestPrelaunchEventsRefusedForNonOwnerRole(t *testing.T) {
	owner, ctx := openTest(t)
	role := fmt.Sprintf("fabric_rt_runner_%d", os.Getpid())
	for _, q := range []string{
		"CREATE ROLE " + role + " NOLOGIN",
		"GRANT ALL ON ALL TABLES IN SCHEMA public TO " + role,
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO " + role,
	} {
		if _, err := owner.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		owner.pool.Exec(ctx, "DROP OWNED BY "+role) //nolint:errcheck
		owner.pool.Exec(ctx, "DROP ROLE "+role)     //nolint:errcheck
	})
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("role", role) // a startup parameter: every connection runs as the runner role
	u.RawQuery = q.Encode()
	runner, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	var who string
	if err := runner.pool.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != role {
		t.Fatalf("runner store runs as %q (%v), want %s", who, err, role)
	}
	mk(t, owner, ctx, "m_pr2")
	ok(t, owner, ctx, "chat", "admit-pr2", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_pr2"})
	fence := startAttempt(t, owner, ctx, "m_pr2", "r_pr2_1")
	// The actor string is the caller's claim; the role is not.
	_, err = runner.Execute(ctx, "fabd", "fin-forged-pr2", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_pr2",
		Attempt: "r_pr2_1", Fence: fence, Outcome: "abandoned", Prelaunch: true})
	if sqlCode(err) != "42501" {
		t.Fatalf("a non-owner role wrote a pre-launch abandonment: %v", err)
	}
	// Control: the same role may finish an attempt without the flags.
	if _, err := runner.Execute(ctx, "runner", "fin-plain-pr2", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_pr2",
		Attempt: "r_pr2_1", Fence: fence, Outcome: "checkpoint"}); err != nil {
		t.Fatalf("control: the runner role cannot finish its own attempt: %v", err)
	}
}
