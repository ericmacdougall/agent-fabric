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
		// Red-team H1 on 193c0a1: the gate trusts these rows, so a runner
		// must not write them directly.
		{"runner", runner, "UPDATE attempts SET state = 'FAILED_DEFINITE', lease_until = NULL"},
		{"runner", runner, "UPDATE attempts SET host = 'elsewhere'"},
		{"runner", runner, "UPDATE missions SET not_before = NULL"},
		{"runner", runner, "UPDATE slots SET cap = 99"},
		{"runner", runner, "DELETE FROM waits"},
		{"runner", runner, "INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response) VALUES ('fabd', 'm_x', 'c_x', 'WAKE', 'x', 'OK', '{}')"},
		{"runner", runner, "DELETE FROM jobs"},
		// Review N2 on aecf3fa: the operator exit is not the runner's.
		{"runner", runner, "INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor) VALUES ('e_x', 'QUARANTINED', 'RESOLVED_APPLIED', 'r', 'e', 'runner')"},
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
	ok(t, app, ctx, "fabd", "ack-rl2", domain.Command{Kind: domain.CmdAckStart, Mission: "m_rl", Attempt: "r_rl_2", Fence: fence})
	mk(t, app, ctx, "m_idle")
	// Red-team J4 on eef7ca7: a runner raises open asks for a running
	// mission only; answers are the app role's.
	if _, err := runner.pool.Exec(ctx, `INSERT INTO asks (id, mission_id, kind, question, source) VALUES ('a_rl', 'm_rl', 'permission', 'q', 'GoodQA goodqa#14922')`); err != nil {
		t.Fatalf("control: the runner cannot raise an ask: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO asks (id, mission_id, kind, question, source, state, answer, answered_by, answered_at, answer_source) VALUES ('a_rl2', 'm_rl', 'permission', 'q', 's', 'answered', 'yes', 'eric', now(), 'forged')`,
		`INSERT INTO asks (id, mission_id, kind, question, source, state, withdrawn_reason) VALUES ('a_rl3', 'm_rl', 'permission', 'q', 's', 'withdrawn', 'x')`,
		`INSERT INTO asks (id, mission_id, kind, question, source) VALUES ('a_rl4', 'm_idle', 'permission', 'q', 's')`,
		`INSERT INTO asks (id, mission_id, kind, question, source) VALUES ('a_rl5', NULL, 'permission', 'q', 's')`,
		`UPDATE asks SET state = 'answered', answer = 'yes', answered_by = 'eric', answered_at = now(), answer_source = 'forged' WHERE id = 'a_rl'`,
		// Nit on eef7ca7: jobs only for the runner's own running attempt.
		`INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ('j_rl9', 'm_rl', 'r_rl_1', 1, '[]', '/x', 'REQUESTED')`,
		`INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ('j_rl8', 'm_idle', 'r_rl_2', 2, '[]', '/x', 'REQUESTED')`,
	} {
		if _, err := runner.pool.Exec(ctx, q); sqlCode(err) != "42501" {
			t.Errorf("runner: %q gave %v, want 42501", q, err)
		}
	}
	// The runner's own write paths still work: a job request and an ask.
	if _, err := runner.pool.Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ('j_rl', 'm_rl', 'r_rl_2', $1, '[]', '/x', 'REQUESTED')`, fence); err != nil {
		t.Fatalf("control: the runner cannot request a job: %v", err)
	}
	if _, err := runner.pool.Exec(ctx, `UPDATE jobs SET state = 'STARTED' WHERE id = 'j_rl'`); sqlCode(err) != "42501" {
		t.Errorf("runner: marking a job STARTED gave %v, want 42501 (the scheduler starts jobs)", err)
	}
	if _, err := app.pool.Exec(ctx, `UPDATE jobs SET state = 'STARTED' WHERE id = 'j_rl'`); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.pool.Exec(ctx, `UPDATE jobs SET state = 'EXITED', exit_code = 0, ended_at = now() WHERE id = 'j_rl'`); err != nil {
		t.Fatalf("control: the job watcher cannot record its exit: %v", err)
	}
	for _, q := range []string{
		"UPDATE jobs SET woken = true WHERE id = 'j_rl'",
		"UPDATE jobs SET state = 'FAILED_TO_START' WHERE id = 'j_rl'",
		"INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state, ended_at) VALUES ('j_rl2', 'm_rl', 'r_rl_2', 1, '[]', '/x', 'EXITED', now())",
		"INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ('j_rl3', 'm_rl', 'r_rl_2', 1, '[]', '/x', 'STARTED')",
	} {
		if _, err := runner.pool.Exec(ctx, q); sqlCode(err) != "42501" {
			t.Errorf("runner: %q gave %v, want 42501", q, err)
		}
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

// Main's follow-up to H1: the runner keeps UPDATE on effects for dispatch,
// but it never approves one.
func TestRunnerCannotApproveEffects(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := openAs(t, ctx, r.Runner())
	mk(t, owner, ctx, "m_ap")
	ok(t, owner, ctx, "chat", "admit-ap", domain.Command{Kind: domain.CmdAdmitMission, Mission: "m_ap"})
	if f := startAttempt(t, owner, ctx, "m_ap", "r_ap_1"); f != 1 {
		t.Fatalf("fence %d, want 1", f)
	}
	ok(t, owner, ctx, "fabd", "ack-ap", domain.Command{Kind: domain.CmdAckStart, Mission: "m_ap", Attempt: "r_ap_1", Fence: 1})
	if _, err := owner.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state)
		VALUES ('e_ap', 'm_ap', 'r_ap_1', 1, 'comment', 's', 'k_ap', '{}', 'sha', 'APPROVAL_REQUIRED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.pool.Exec(ctx, `INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, approval_sha, approved_by) VALUES
		('e_ap3', 'm_ap', 'r_ap_1', 1, 'comment', 's', 'k_ap3', '{}', 'sha', 'FAILED_DEFINITE', NULL, NULL),
		('e_ok', 'm_ap', 'r_ap_1', 1, 'comment', 's', 'k_ok', '{}', 'sha', 'FAILED_DEFINITE', 'sha', 'eric')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE effects SET state = 'APPROVED' WHERE id = 'e_ap'",
		"UPDATE effects SET approval_sha = 'sha', approved_by = 'runner' WHERE id = 'e_ap'",
		"INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state) VALUES ('e_ap2', 'm_ap', 'r_ap_1', 1, 'comment', 's', 'k_ap2', '{}', 'sha', 'APPROVED')",
		"UPDATE effects SET state = 'APPROVED' WHERE id = 'e_ap3'",                // retry of a never-approved effect
		"UPDATE effects SET request = '{\"body\":\"swapped\"}' WHERE id = 'e_ok'", // content after approval
	} {
		if _, err := runner.pool.Exec(ctx, q); sqlCode(err) != "42501" {
			t.Errorf("runner: %q gave %v, want 42501", q, err)
		}
	}
	// Control: Broker.Retry as the runner re-arms an approved effect.
	if _, err := runner.pool.Exec(ctx, "UPDATE effects SET state = 'APPROVED', updated_at = now() WHERE id = 'e_ok' AND state = 'FAILED_DEFINITE'"); err != nil {
		t.Fatalf("control: the runner cannot retry an approved effect: %v", err)
	}
}

// A deployment whose runner role was made by 193c0a1 (DML on every table):
// CheckRunnerRole refuses it, and re-applying the roles narrows it.
func TestRolesUpgradeNarrowsRunner(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := openAs(t, ctx, r.Runner())
	// Each grant alone is enough to be refused.
	for _, g := range []string{"UPDATE ON attempts", "UPDATE ON missions", "UPDATE ON slots", "DELETE ON waits", "INSERT ON commands", "INSERT ON effect_resolutions"} {
		if _, err := owner.pool.Exec(ctx, "GRANT "+g+" TO "+r.Runner()); err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckRunnerRole(ctx); err == nil {
			t.Errorf("CheckRunnerRole passed a runner with %s", g)
		}
		if _, err := owner.pool.Exec(ctx, "REVOKE "+g+" FROM "+r.Runner()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.pool.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "+r.Runner()+"; REVOKE INSERT, UPDATE, DELETE ON hold_commands FROM "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Fatal("CheckRunnerRole passed a runner that can UPDATE attempts")
	}
	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("after re-applying the roles: %v", err)
	}
	if _, err := runner.pool.Exec(ctx, "UPDATE attempts SET host = 'elsewhere'"); sqlCode(err) != "42501" {
		t.Fatalf("after re-applying the roles the runner's UPDATE attempts gave %v, want 42501", err)
	}
}

// A runner may create temporary tables, and pg_temp is searched first, so
// an unqualified fabric_settings or events in a guard could be shadowed by
// one the runner names itself the scheduler in.
func TestRunnerCannotShadowGuardTables(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := openAs(t, ctx, r.Runner())
	mk(t, owner, ctx, "m_sh")
	if _, err := owner.pool.Exec(ctx, `INSERT INTO jobs (id, mission_id, attempt_id, fence, argv, rundir, state) VALUES ('j_sh', 'm_sh', 'r_sh_1', 1, '[]', '/x', 'REQUESTED')`); err != nil {
		t.Fatal(err)
	}
	conn, err := runner.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "CREATE TEMP TABLE fabric_settings (key text, value text); INSERT INTO fabric_settings VALUES ('scheduler_role', '"+r.Runner()+"')"); err != nil {
		t.Fatalf("setup: the runner cannot create its shadow table: %v", err)
	}
	_, err = conn.Exec(ctx, "UPDATE jobs SET state = 'STARTED' WHERE id = 'j_sh'")
	if sqlCode(err) != "42501" {
		t.Fatalf("a runner with a shadow fabric_settings started a job: %v, want 42501", err)
	}
}
