package store

import (
	"strings"
	"testing"
)

// Requires the real isolated stage/TestMain. These are not protocol-double or
// compile-only assertions and must not be reported passed without that runtime.
func TestRolePasswordsPreserveInheritanceAndScheduler(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	appPW := "synthetic-only-" + strings.Repeat("A", 32)
	runnerPW := "synthetic-only-" + strings.Repeat("R", 32)
	for _, inherit := range []bool{true, false} {
		flag := "NOINHERIT"
		if inherit {
			flag = "INHERIT"
		}
		if _, err := owner.pool.Exec(ctx, "ALTER ROLE "+r.App()+" "+flag+"; ALTER ROLE "+r.Runner()+" "+flag); err != nil {
			t.Fatal("inheritance fixture setup failed")
		}
		if err := owner.ApplyRoles(ctx, r, appPW, runnerPW); err != nil {
			t.Fatal("password apply failed; raw error suppressed")
		}
		for _, role := range []string{r.App(), r.Runner()} {
			var got, login bool
			if err := owner.pool.QueryRow(ctx, "SELECT rolinherit,rolcanlogin FROM pg_roles WHERE rolname=$1", role).Scan(&got, &login); err != nil || got != inherit || !login {
				t.Error("password apply changed inheritance or failed LOGIN")
			}
		}
		var schedulerLogin, appMember, runnerMember bool
		if err := owner.pool.QueryRow(ctx, "SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", r.Scheduler()).Scan(&schedulerLogin); err != nil || schedulerLogin {
			t.Error("scheduler became LOGIN")
		}
		if err := owner.pool.QueryRow(ctx, "SELECT pg_has_role($1,$2,'MEMBER'),pg_has_role($3,$2,'MEMBER')", r.App(), r.Scheduler(), r.Runner()).Scan(&appMember, &runnerMember); err != nil || !appMember || runnerMember {
			t.Error("scheduler membership changed")
		}
	}
	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal("NOLOGIN apply failed")
	}
	var logins int
	if err := owner.pool.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=ANY($1) AND rolcanlogin", []string{r.App(), r.Runner(), r.Scheduler()}).Scan(&logins); err != nil || logins != 0 {
		t.Error("empty passwords did not preserve NOLOGIN")
	}
}
