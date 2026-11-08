package store

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
)

func authorityAttempt(t *testing.T, owner *Store, ctx context.Context, id string, terminal bool) (string, int64) {
	t.Helper()
	mission := domain.MissionID(id)
	attempt := domain.AttemptID("r_" + id)
	mk(t, owner, ctx, mission)
	ok(t, owner, ctx, "chat", "admit:"+id, domain.Command{Kind: domain.CmdAdmitMission, Mission: mission})
	fence := startAttempt(t, owner, ctx, mission, attempt)
	if terminal {
		ok(t, owner, ctx, "fabd", "finish:"+id, domain.Command{Kind: domain.CmdFinishAttempt, Mission: mission,
			Attempt: attempt, Fence: fence, Outcome: "abandoned", Prelaunch: true})
	}
	return string(attempt), fence
}

func authorityHeldReservation(t *testing.T, owner *Store, ctx context.Context, id string) {
	t.Helper()
	attempt, fence := authorityAttempt(t, owner, ctx, "m_"+id, false)
	tx, err := owner.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	for _, q := range []string{
		"INSERT INTO public.resource_capacities (resource, unit, capacity, held) VALUES ('cap_" + id + "', 'slots', 10, 5)",
		"INSERT INTO public.hold_commands (key, request_sha256) VALUES ('hold_" + id + "', 'sha')",
		"INSERT INTO public.reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, expires_at) VALUES ('res_" + id + "', 'hold_" + id + "', '" + attempt + "', " + int64String(fence) + ", 'cap_" + id + "', 'slots', 5, 1, now() + interval '1 hour')",
		"INSERT INTO public.reservation_events (reservation_id, n, state) VALUES ('res_" + id + "', 1, 'HELD')",
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("reservation fixture %q: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func int64String(v int64) string {
	return fmtInt(v)
}

func fmtInt(v int64) string {
	// Kept local so these fixtures do not alter any production API.
	return strconv.FormatInt(v, 10)
}

func TestAuthorityTempTablesCannotShadowReservationBackstops(t *testing.T) {
	t.Run("attempts", func(t *testing.T) {
		owner, ctx := openTest(t)
		r := testRoles(t, owner, ctx)
		attempt, fence := authorityAttempt(t, owner, ctx, "m_shadow_attempt", true)
		for _, q := range []string{
			"INSERT INTO public.resource_capacities (resource, unit, capacity) VALUES ('cap_shadow_attempt', 'slots', 10)",
			"INSERT INTO public.hold_commands (key, request_sha256) VALUES ('hold_shadow_attempt', 'sha')",
		} {
			if _, err := owner.pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		app := openAs(t, ctx, r.App())
		conn, err := app.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "CREATE TEMP TABLE attempts (id text, fence bigint, state text)"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO pg_temp.attempts VALUES ($1, $2, 'RUNNING')", attempt, fence); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, expires_at) VALUES ('res_shadow_attempt', 'hold_shadow_attempt', $1, $2, 'cap_shadow_attempt', 'slots', 1, 1, now() + interval '1 hour')", attempt, fence)
		if sqlCode(err) != "23514" {
			t.Errorf("temporary live attempt shadowed the terminal public attempt: %v, want 23514", err)
		}
		tx.Rollback(ctx) //nolint:errcheck
	})

	t.Run("resource_capacities and reservations", func(t *testing.T) {
		owner, ctx := openTest(t)
		r := testRoles(t, owner, ctx)
		if _, err := owner.pool.Exec(ctx, "INSERT INTO public.resource_capacities (resource, unit, capacity) VALUES ('cap_shadow_sum', 'slots', 10)"); err != nil {
			t.Fatal(err)
		}
		app := openAs(t, ctx, r.App())
		conn, err := app.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `CREATE TEMP TABLE resource_capacities (resource text, held bigint);
			CREATE TEMP TABLE reservations (resource text, amount bigint, state text);
			INSERT INTO pg_temp.resource_capacities VALUES ('cap_shadow_sum', 7);
			INSERT INTO pg_temp.reservations VALUES ('cap_shadow_sum', 7, 'HELD')`); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "UPDATE public.resource_capacities SET held = 7 WHERE resource = 'cap_shadow_sum'"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); sqlCode(err) != "23514" {
			t.Errorf("temporary capacity and live holds hid a public capacity mismatch: %v, want 23514", err)
		}
	})

	t.Run("reservation_events", func(t *testing.T) {
		owner, ctx := openTest(t)
		r := testRoles(t, owner, ctx)
		authorityHeldReservation(t, owner, ctx, "shadow_history")
		app := openAs(t, ctx, r.App())
		conn, err := app.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "CREATE TEMP TABLE reservation_events (reservation_id text, n smallint, state text); INSERT INTO pg_temp.reservation_events VALUES ('res_shadow_history', 1, 'HELD'), ('res_shadow_history', 2, 'RELEASED')"); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			"UPDATE public.reservations SET state = 'RELEASED', closed_at = now() WHERE id = 'res_shadow_history'",
			"UPDATE public.resource_capacities SET held = 0 WHERE resource = 'cap_shadow_history'",
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); sqlCode(err) != "23514" {
			t.Errorf("temporary terminal event hid missing public history: %v, want 23514", err)
		}
	})

	t.Run("reservations child check", func(t *testing.T) {
		owner, ctx := openTest(t)
		r := testRoles(t, owner, ctx)
		authorityHeldReservation(t, owner, ctx, "shadow_child")
		app := openAs(t, ctx, r.App())
		conn, err := app.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "CREATE TEMP TABLE reservations (id text, state text); INSERT INTO pg_temp.reservations VALUES ('res_shadow_child', 'RELEASED')"); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO public.reservation_events (reservation_id, n, state) VALUES ('res_shadow_child', 2, 'RELEASED')"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); sqlCode(err) != "23514" {
			t.Errorf("temporary reservation state legitimized an inconsistent public event: %v, want 23514", err)
		}
	})
}

const authorityRunnerPassword = "authority-runner-password"

func openAuthorityRunner(t *testing.T, ctx context.Context, r Roles, extra url.Values) *Store {
	t.Helper()
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(r.Runner(), authorityRunnerPassword)
	q := u.Query()
	q.Del("role")
	q.Del("session_authorization")
	q.Del("options")
	for key, values := range extra {
		q[key] = append([]string(nil), values...)
	}
	u.RawQuery = q.Encode()
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func authorityRunnerLogin(t *testing.T, owner *Store, ctx context.Context, r Roles) *Store {
	t.Helper()
	if err := owner.ApplyRoles(ctx, r, "", authorityRunnerPassword); err != nil {
		t.Fatal(err)
	}
	return openAuthorityRunner(t, ctx, r, nil)
}

func TestAuthorityRunnerRequiresDirectAuthentication(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("genuine runner login failed its check: %v", err)
	}
	var sessionUser, currentUser string
	if err := runner.pool.QueryRow(ctx, "SELECT session_user, current_user").Scan(&sessionUser, &currentUser); err != nil {
		t.Fatal(err)
	}
	if sessionUser != r.Runner() || currentUser != r.Runner() {
		t.Fatalf("direct login identities = %s/%s, want %s", sessionUser, currentUser, r.Runner())
	}

	indirect := openAs(t, ctx, r.Runner())
	if err := indirect.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "direct runner authentication") {
		t.Errorf("owner DSN with role=%s passed direct-authentication validation: %v", r.Runner(), err)
	}

	// Exercise the SQL defense independently of DSN validation: every session
	// authenticates as the owner, then switches role after connection startup.
	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, conn *pgconn.PgConn) error {
		return conn.Exec(ctx, "SET ROLE "+r.Runner()).Close()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sqlIndirect := &Store{pool: pool}
	t.Cleanup(sqlIndirect.Close)
	if err := sqlIndirect.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "session user differs") {
		t.Errorf("SQL session_user/current_user check accepted an indirect role: %v", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"role", r.Runner()},
		{"session_authorization", r.Runner()},
		{"options", "-c role=" + r.Runner()},
		{"options", "--session_authorization=" + r.Runner()},
	} {
		st := openAuthorityRunner(t, ctx, r, url.Values{tc.key: []string{tc.value}})
		if err := st.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "direct runner authentication") {
			t.Errorf("startup %s=%q passed direct-authentication validation: %v", tc.key, tc.value, err)
		}
	}
}

func TestAuthorityRunnerRejectsAllEscalatedPrivileges(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)

	for _, grant := range []string{
		"UPDATE ON public.resource_capacities",
		"INSERT ON public.reservations",
		"INSERT ON public.reservation_events",
		"DELETE ON public.usage_receipts",
		"INSERT ON public.fabric_schema",
		"TRUNCATE ON public.events",
	} {
		if _, err := owner.pool.Exec(ctx, "GRANT "+grant+" TO "+r.Runner()); err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckRunnerRole(ctx); err == nil {
			t.Errorf("CheckRunnerRole accepted runner grant %s", grant)
		}
		if _, err := owner.pool.Exec(ctx, "REVOKE "+grant+" FROM "+r.Runner()); err != nil {
			t.Fatal(err)
		}
	}

	// A column grant is real write authority even with no table-level UPDATE.
	if _, err := owner.pool.Exec(ctx, "GRANT UPDATE (host) ON public.attempts TO "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Error("CheckRunnerRole accepted column-level UPDATE on attempts")
	}
	if _, err := runner.pool.Exec(ctx, "UPDATE public.attempts SET host = 'forged' WHERE false"); err != nil {
		t.Fatalf("control: column-level UPDATE was not writable: %v", err)
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE UPDATE (host) ON public.attempts FROM "+r.Runner()); err != nil {
		t.Fatal(err)
	}

	// An auto-updatable view is another write path around table-level checks.
	if _, err := owner.pool.Exec(ctx, `CREATE VIEW public.authority_attempts_view AS
		SELECT id, host, state FROM public.attempts;
		GRANT UPDATE ON public.authority_attempts_view TO `+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Error("CheckRunnerRole accepted UPDATE on an auto-updatable attempts view")
	}
	if _, err := runner.pool.Exec(ctx, "UPDATE public.authority_attempts_view SET host = 'forged' WHERE false"); err != nil {
		t.Fatalf("control: auto-updatable view was not writable: %v", err)
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE UPDATE ON public.authority_attempts_view FROM "+r.Runner()+"; DROP VIEW public.authority_attempts_view"); err != nil {
		t.Fatal(err)
	}

	for _, attribute := range []struct{ on, off string }{
		{"SUPERUSER", "NOSUPERUSER"},
		{"CREATEDB", "NOCREATEDB"},
		{"CREATEROLE", "NOCREATEROLE"},
		{"BYPASSRLS", "NOBYPASSRLS"},
		{"REPLICATION", "NOREPLICATION"},
	} {
		if _, err := owner.pool.Exec(ctx, "ALTER ROLE "+r.Runner()+" "+attribute.on); err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckRunnerRole(ctx); err == nil {
			t.Errorf("CheckRunnerRole accepted runner attribute %s", attribute.on)
		}
		if _, err := owner.pool.Exec(ctx, "ALTER ROLE "+r.Runner()+" "+attribute.off); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := owner.pool.Exec(ctx, "GRANT pg_write_all_data TO "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Error("CheckRunnerRole accepted inherited pg_write_all_data authority")
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE pg_write_all_data FROM "+r.Runner()); err != nil {
		t.Fatal(err)
	}

	// NOINHERIT does not make an unsafe membership safe: the runner can SET
	// ROLE to the member role, so both direct and indirect memberships count.
	delegated := r.Prefix + "_authority_admin"
	if _, err := owner.pool.Exec(ctx, "CREATE ROLE "+delegated+" NOLOGIN CREATEROLE; GRANT "+delegated+" TO "+r.Runner()+" WITH INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Error("CheckRunnerRole accepted a NOINHERIT membership with CREATEROLE")
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE "+delegated+" FROM "+r.Runner()+"; DROP ROLE "+delegated); err != nil {
		t.Fatal(err)
	}

	if _, err := owner.pool.Exec(ctx, "GRANT CREATE ON SCHEMA public TO "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil {
		t.Error("CheckRunnerRole accepted CREATE on the authority schema")
	}
	if _, err := owner.pool.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("restored runner role failed its check: %v", err)
	}
}

func TestAuthorityRunnerCannotReadReviewGateRequests(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)
	if _, err := owner.pool.Exec(ctx, "GRANT SELECT ON public.review_gate_requests TO "+r.Runner()); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.pool.Exec(ctx, "SELECT canonical_proposal FROM public.review_gate_requests WHERE false"); err != nil {
		t.Fatalf("control: explicit review-gate SELECT was not readable: %v", err)
	}
	if err := owner.ApplyRoles(ctx, r, "", authorityRunnerPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.pool.Exec(ctx, "SELECT canonical_proposal FROM public.review_gate_requests WHERE false"); sqlCode(err) != "42501" {
		t.Errorf("ApplyRoles exposed review-gate plaintext arguments: %v, want 42501", err)
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("runner without review-gate SELECT failed its check: %v", err)
	}
}

// A runner holding SELECT on a review table must fail its own check, not only
// lose the grant at the next ApplyRoles (goodqa#15235).
func TestAuthorityCheckRefusesRunnerReadingReviewTables(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)
	for _, table := range []string{"review_gate_requests", "review_gate_results"} {
		if _, err := owner.pool.Exec(ctx, "GRANT SELECT ON public."+table+" TO "+r.Runner()); err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "review-table SELECT") {
			t.Errorf("CheckRunnerRole accepted SELECT on %s: %v", table, err)
		}
		if _, err := owner.pool.Exec(ctx, "REVOKE SELECT ON public."+table+" FROM "+r.Runner()); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.CheckRunnerRole(ctx); err != nil {
		t.Fatalf("restored runner role failed its check: %v", err)
	}
}

// A NOINHERIT membership inherits nothing, but the runner can SET ROLE to it,
// so a write grant held only by that role still counts against the runner.
func TestAuthorityCheckScansNoInheritMemberRoleWrites(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	runner := authorityRunnerLogin(t, owner, ctx, r)
	delegated := r.Prefix + "_noinherit_writer"
	if _, err := owner.pool.Exec(ctx, "CREATE ROLE "+delegated+" NOLOGIN; GRANT UPDATE ON public.attempts TO "+delegated+"; GRANT "+delegated+" TO "+r.Runner()+" WITH INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.pool.Exec(context.Background(), "REVOKE "+delegated+" FROM "+r.Runner()+"; REVOKE ALL ON public.attempts FROM "+delegated+"; DROP ROLE IF EXISTS "+delegated) //nolint:errcheck
	})
	if _, err := runner.pool.Exec(ctx, "UPDATE public.attempts SET host = host WHERE false"); sqlCode(err) != "42501" {
		t.Fatalf("control: the NOINHERIT role must not leak UPDATE into the runner session: %v", err)
	}
	if err := runner.CheckRunnerRole(ctx); err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Errorf("CheckRunnerRole ignored a NOINHERIT member role that can UPDATE attempts: %v", err)
	}
}
