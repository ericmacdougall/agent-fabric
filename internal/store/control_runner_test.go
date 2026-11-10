package store

import (
	"context"
	"strings"
	"testing"
)

// goodqa#15623 AC-7: the runner check covers schema control. A runner with CREATE
// on control, any privilege on a control table, or a membership of a role that
// has one fails; a drifted control role does not stop the runner check (so it
// cannot stop fabd or a rollback).
func TestRoleSafetyCoversControl(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	if _, err := owner.pool.Exec(ctx, `CREATE SCHEMA control; CREATE TABLE control.t_ac7 (a int)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS control CASCADE`) }) //nolint:errcheck
	runner := authorityRunnerLogin(t, owner, ctx, r)
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("control: a clean runner failed its check with a control schema present: %v", err)
	}
	for name, c := range map[string]struct{ grant, revoke string }{
		"create":        {"GRANT CREATE ON SCHEMA control TO " + r.Runner(), "REVOKE CREATE ON SCHEMA control FROM " + r.Runner()},
		"select":        {"GRANT USAGE ON SCHEMA control TO " + r.Runner() + "; GRANT SELECT ON control.t_ac7 TO " + r.Runner(), "REVOKE SELECT ON control.t_ac7 FROM " + r.Runner()},
		"column-select": {"GRANT USAGE ON SCHEMA control TO " + r.Runner() + "; GRANT SELECT (a) ON control.t_ac7 TO " + r.Runner(), "REVOKE SELECT (a) ON control.t_ac7 FROM " + r.Runner()},
		"column-ins":    {"GRANT USAGE ON SCHEMA control TO " + r.Runner() + "; GRANT INSERT (a) ON control.t_ac7 TO " + r.Runner(), "REVOKE INSERT (a) ON control.t_ac7 FROM " + r.Runner()},
	} {
		if _, err := owner.pool.Exec(ctx, c.grant); err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "control schema") {
			t.Errorf("%s: CheckRunnerRole = %v, want a refusal naming the control schema", name, err)
		}
		if _, err := owner.pool.Exec(ctx, c.revoke); err != nil {
			t.Fatal(err)
		}
	}
	// A NOINHERIT member's column SELECT is assumable and must be refused.
	member := r.Runner() + "_col"
	if _, err := owner.pool.Exec(ctx, "CREATE ROLE "+member+" NOLOGIN; GRANT USAGE ON SCHEMA control TO "+member+"; GRANT SELECT (a) ON control.t_ac7 TO "+member+"; GRANT "+member+" TO "+r.Runner()+" WITH INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "control schema") {
		t.Error("NOINHERIT member column SELECT passed runner check")
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE "+member+" FROM "+r.Runner()+"; REVOKE ALL ON control.t_ac7 FROM "+member+"; REVOKE ALL ON SCHEMA control FROM "+member+"; DROP ROLE "+member); err != nil {
		t.Fatal(err)
	}
	// inherited: a NOINHERIT membership of a role holding CREATE on control counts
	if _, err := owner.pool.Exec(ctx, `CREATE ROLE ac7_member NOLOGIN; GRANT CREATE ON SCHEMA control TO ac7_member; GRANT ac7_member TO `+r.Runner()+` WITH INHERIT FALSE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.pool.Exec(context.Background(), `REVOKE ac7_member FROM `+r.Runner()+`; REVOKE ALL ON SCHEMA control FROM ac7_member; DROP ROLE IF EXISTS ac7_member`) //nolint:errcheck
	})
	if err := runner.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "control schema") {
		t.Errorf("inherited: CheckRunnerRole = %v, want a refusal naming the control schema", err)
	}
	if _, err := owner.pool.Exec(ctx, `REVOKE ac7_member FROM `+r.Runner()); err != nil {
		t.Fatal(err)
	}
	// a drifted control role (CREATE on control held by some other role) does not affect the runner check
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Errorf("a control role holding CREATE stopped the runner check: %v (a drifted control role must never stop fabd or a rollback)", err)
	}
	owner.pool.Exec(ctx, `REVOKE ALL ON SCHEMA control FROM ac7_member; DROP ROLE IF EXISTS ac7_member`) //nolint:errcheck
}
