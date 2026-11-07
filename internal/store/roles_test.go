package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"fabric/internal/domain"
)

// goodqa#14929 and red-team G2: the app and runner roles are not owners or
// superusers, so the database backstops hold against a misbehaving client,
// and only the app role (a scheduler member) writes the scheduler events.

// openAs opens a store whose sessions run as role (a startup parameter on
// the admin DSN, so no password is needed in tests).
func openAs(t *testing.T, ctx context.Context, role string) *Store {
	t.Helper()
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("role", role)
	u.RawQuery = q.Encode()
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func testRoles(t *testing.T, owner *Store, ctx context.Context) Roles {
	t.Helper()
	r := Roles{Prefix: fmt.Sprintf("fabric_t%d", os.Getpid())}
	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			owner.pool.Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			owner.pool.Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	return r
}

func TestRolesLeastPrivilege(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	app, runner := openAs(t, ctx, r.App()), openAs(t, ctx, r.Runner())
	for _, c := range []struct {
		who  string
		st   *Store
		stmt string
	}{
		{"app", app, "TRUNCATE events"},
		{"app", app, "ALTER TABLE events ADD COLUMN x int"},
		{"app", app, "ALTER TABLE events DISABLE TRIGGER ALL"},
		{"app", app, "SET session_replication_role = replica"},
		{"app", app, "CREATE TABLE public.squat (x int)"},
		{"app", app, "UPDATE fabric_settings SET value = 'x'"},
		{"runner", runner, "INSERT INTO hold_commands (key, request_sha256) VALUES ('hold:r_x_1', 'x')"},
		{"runner", runner, "UPDATE resource_capacities SET held = 0"},
		{"runner", runner, "UPDATE fabric_settings SET value = 'x'"},
		{"runner", runner, "TRUNCATE events"},
	} {
		if _, err := c.st.pool.Exec(ctx, c.stmt); sqlCode(err) != "42501" {
			t.Errorf("%s: %q gave %v, want 42501", c.who, c.stmt, err)
		}
	}
	// The app role runs the scheduler: ordinary commands and the reserved
	// pre-launch events both go through.
	mk(t, app, ctx, "m_rl")
	ok(t, app, ctx, "chat", "admit-rl", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_rl"})
	fence := startAttempt(t, app, ctx, "m_rl", "r_rl_1")
	ok(t, app, ctx, "fabd", "fin-rl", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_rl", Attempt: "r_rl_1",
		Fence: fence, Outcome: "abandoned", Prelaunch: true})
	// The runner role cannot write them, whatever actor it claims.
	fence = startAttempt(t, app, ctx, "m_rl", "r_rl_2")
	_, err := runner.Execute(ctx, "fabd", "fin-rl-forged", domain.Command{Kind: domain.CmdFinishAttempt, Mission: "m_rl",
		Attempt: "r_rl_2", Fence: fence, Outcome: "abandoned", Prelaunch: true})
	if sqlCode(err) != "42501" {
		t.Fatalf("the runner role wrote a pre-launch abandonment: %v", err)
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("the runner role failed its own check: %v", err)
	}
	for name, st := range map[string]*Store{"app": app, "owner": owner} {
		if err := st.CheckRunnerRole(ctx); err == nil {
			t.Errorf("CheckRunnerRole accepted the %s role as a runner", name)
		}
	}
}
