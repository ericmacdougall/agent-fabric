package control

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	exportevents "fabric/internal/control/export"
	"fabric/internal/storetest"
)

func TestExportRoleJoinedColumns(t *testing.T) {
	p, ctx, r := setupRoles(t)
	for _, q := range []string{"SELECT id,ticket FROM missions", "SELECT consumer,last_id FROM outbox_cursors"} {
		if err := execAs(t, p, ctx, r.Export(), q); err != nil {
			t.Error("required joined columns unreachable")
		}
	}
	for _, q := range []string{"SELECT * FROM missions", "SELECT repo FROM missions", "SELECT * FROM outbox_cursors", "SELECT updated_at FROM outbox_cursors", "UPDATE missions SET ticket='x'", "DELETE FROM outbox_cursors"} {
		if err := execAs(t, p, ctx, r.Export(), q); code(err) != "42501" {
			t.Errorf("forbidden metadata privilege: SQLSTATE %s", code(err))
		}
	}
	for i := 0; i < 2; i++ {
		mission := fmt.Sprintf("metadata_%d", i)
		if _, err := p.Exec(ctx, `INSERT INTO missions(id,ticket,state) VALUES($1,'GoodQA goodqa#15623','{}');`, mission); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO events(mission_id,seq,kind,actor,command_id,payload) VALUES($1,1,'MISSION_ADMITTED','test','metadata','{}')`, mission); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO outbox(mission_id,seq,kind,payload) VALUES($1,1,'MISSION_ADMITTED','{}')`, mission); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Exec(ctx, `INSERT INTO outbox_cursors(consumer,last_id) VALUES('sched.hold.metadata_1',0)`); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx) //nolint:errcheck
	if _, err = c.Exec(ctx, "SET ROLE "+quoteID(r.Export())); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	res, err := exportevents.Export(ctx, c, "", &output)
	if err != nil || res.Events != 2 || !strings.Contains(output.String(), `"ticket":"goodqa#15623"`) || !strings.Contains(output.String(), "stream held, event withheld") {
		t.Fatal("intended export role did not export normal ticket and held events")
	}
	// Seed exactly the drift table REVOKE is documented to remove, then reapply.
	if _, err := p.Exec(ctx, "GRANT SELECT (repo) ON missions TO "+quoteID(r.Export())); err != nil {
		t.Fatal(err)
	}
	problems, _, err := VerifyRoles(ctx, p, r)
	if err != nil || len(problems) == 0 {
		t.Fatal("column drift invisible to verifier")
	}
	if _, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil {
		t.Fatal(err)
	}
	if err := execAs(t, p, ctx, r.Export(), "SELECT repo FROM missions"); code(err) != "42501" {
		t.Fatal("reapply left forbidden column reachable")
	}
	problems, _, err = VerifyRoles(ctx, p, r)
	if err != nil || len(problems) != 0 {
		t.Fatal("reapplied matrix not clean")
	}
	if changed, err := ApplyRoles(ctx, p, r, Passwords{}); err != nil || changed {
		t.Fatal("reapply not idempotent")
	}
}

type sensitiveSignal struct{ started chan uint32 }

func (s sensitiveSignal) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "ALTER ROLE ") && strings.Contains(d.SQL, " PASSWORD ") {
		select {
		case s.started <- c.PgConn().PID():
		default:
		}
	}
	return ctx
}
func (s sensitiveSignal) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The observer has superuser stats visibility. It only returns matching counts;
// it never retrieves query text, a password, or a credential-bearing error.
func TestControlRolePasswordActivityDuringBlockedAlter(t *testing.T) {
	p, _, r := setupRoles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	synthetic := "synthetic-only-" + strings.Repeat("Q", 32)
	var super bool
	if err := p.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&super); err != nil || !super {
		t.Fatal("observer requires superuser stats visibility")
	}
	for _, protected := range []bool{false, true} {
		locker, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		locktx, err := locker.Begin(ctx)
		if err != nil {
			locker.Release()
			t.Fatal(err)
		}
		var oid uint32
		if err := locktx.QueryRow(ctx, "SELECT oid FROM pg_authid WHERE rolname=$1 FOR UPDATE", r.Export()).Scan(&oid); err != nil {
			locktx.Rollback(ctx)
			locker.Release()
			t.Fatal("cannot lock unique disposable role row")
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			locktx.Rollback(cleanup) //nolint:errcheck
			// Release is idempotent after the explicit successful path below.
			locker.Release()
		})
		blocker := locker.Conn().PgConn().PID()
		signal := sensitiveSignal{started: make(chan uint32, 1)}
		cfg, err := pgxpool.ParseConfig(storetest.DSN)
		if err != nil {
			t.Fatal("actor config failed")
		}
		cfg.MaxConns = 1
		cfg.ConnConfig.Tracer = signal
		actor, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("actor pool failed")
		}
		settingsSQL := "SELECT ARRAY[current_setting('track_activities'), current_setting('log_min_error_statement'), current_setting('log_statement'), current_setting('log_min_duration_statement'), coalesce(current_setting('pg_stat_statements.track_utility', true), '')]"
		var settingsBefore []string
		if err := actor.QueryRow(ctx, settingsSQL).Scan(&settingsBefore); err != nil {
			t.Fatal("initial session settings unavailable")
		}
		result := make(chan error, 1)
		go func() {
			if protected {
				_, err := ApplyRoles(ctx, actor, r, Passwords{Export: synthetic})
				result <- err
				return
			}
			c, err := actor.Acquire(ctx)
			if err != nil {
				result <- err
				return
			}
			defer c.Release()
			tx, err := c.Begin(ctx)
			if err != nil {
				result <- err
				return
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			for _, q := range []string{"SET LOCAL track_activities=on", "SET LOCAL log_min_error_statement=panic", "SET LOCAL log_statement=none", "SET LOCAL log_min_duration_statement=-1", "SET LOCAL pg_stat_statements.track_utility=off"} {
				if _, err := tx.Exec(ctx, q); err != nil {
					result <- fmt.Errorf("baseline protection setup failed")
					return
				}
			}
			escaped, err := c.Conn().PgConn().EscapeString(synthetic)
			if err != nil {
				result <- fmt.Errorf("synthetic escaping failed")
				return
			}
			if _, err := tx.Exec(ctx, "ALTER ROLE "+quoteID(r.Export())+" PASSWORD '"+escaped+"'"); err != nil {
				result <- fmt.Errorf("baseline ALTER failed")
				return
			}
			result <- tx.Commit(ctx)
		}()
		var pid uint32
		select {
		case pid = <-signal.started:
		case <-ctx.Done():
			t.Error("sensitive ALTER was not sent")
		}
		blocked := false
		for ctx.Err() == nil {
			if err := p.QueryRow(ctx, "SELECT $2::int = ANY(pg_blocking_pids($1::int))", pid, blocker).Scan(&blocked); err != nil {
				t.Fatal("blocking check failed")
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Error("sensitive ALTER was not proven blocked on owned role")
		}
		if _, err := p.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
			t.Fatal("fresh observer snapshot failed")
		}
		var matches int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND strpos(query,$2)>0", pid, synthetic).Scan(&matches); err != nil {
			t.Fatal("observer count failed")
		}
		t.Logf("protected=%t matching_activity_count=%d", protected, matches)
		if (!protected && matches == 0) || (protected && matches != 0) {
			t.Error("activity visibility control failed")
		}
		if err := locktx.Rollback(ctx); err != nil {
			t.Fatal("owned row lock cleanup failed")
		}
		locker.Release()
		select {
		case err := <-result:
			if err != nil {
				t.Error("actor operation failed (details suppressed)")
			}
		case <-ctx.Done():
			t.Error("actor failed to finish")
		}
		// A single-connection pool must hand back a restored idle session.
		var settingsAfter []string
		if err := actor.QueryRow(ctx, settingsSQL).Scan(&settingsAfter); err != nil || !reflect.DeepEqual(settingsBefore, settingsAfter) {
			t.Error("pooled session protection settings did not restore")
		}
		actor.Close()
	}
}

func TestControlRolesRefuseUnprotectedActivity(t *testing.T) {
	p, ctx, r := setupRoles(t)
	admin := r.Prefix + "_limited"
	if _, err := p.Exec(ctx, "CREATE ROLE "+quoteID(admin)+" NOLOGIN CREATEROLE; GRANT USAGE ON SCHEMA control TO "+quoteID(admin)+"; GRANT SELECT ON control.ingested_events TO "+quoteID(admin)); err != nil {
		t.Fatal("limited admin setup failed")
	}
	t.Cleanup(func() {
		p.Exec(context.Background(), "REVOKE ALL ON control.ingested_events FROM "+quoteID(admin)+"; REVOKE ALL ON SCHEMA control FROM "+quoteID(admin)+"; DROP ROLE "+quoteID(admin))
	}) //nolint:errcheck
	cfg, err := pgxpool.ParseConfig(storetest.DSN)
	if err != nil {
		t.Fatal("limited config failed")
	}
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		for _, q := range []string{"SET log_min_error_statement=panic", "SET log_statement=none", "SET log_min_duration_statement=-1", "SET pg_stat_statements.track_utility=off", "SET ROLE " + quoteID(admin)} {
			if _, err := c.Exec(ctx, q); err != nil {
				return fmt.Errorf("limited session setup failed")
			}
		}
		return nil
	}
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("limited pool failed")
	}
	defer limited.Close()
	var before string
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err = rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyRoles(ctx, limited, r, Passwords{Export: "synthetic-only-" + strings.Repeat("R", 32)})
	if err == nil || !strings.Contains(err.Error(), "activity tracking") {
		t.Error("unprotected activity was not refused before mutation")
	}
	c, err = p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil || before != after {
		t.Error("refusal changed role state")
	}
	var tracking string
	if err := limited.QueryRow(ctx, "SELECT current_setting('track_activities')").Scan(&tracking); err != nil || tracking != "on" {
		t.Error("refused session did not restore safely")
	}
}

// Inject restoration corruption or a closed postcommit connection without
// logging query text. Both paths must report committed state and discard.
type completionFault struct {
	loseAfterCommit bool
	completed       bool
}

func (f *completionFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (f *completionFault) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData) {
	if !f.completed && d.CommandTag.String() == "COMMIT" && d.Err == nil {
		f.completed = true
		if f.loseAfterCommit {
			c.Close(ctx)
		} else {
			c.Exec(ctx, "SET track_activities=off")
		} //nolint:errcheck
	}
}
func TestControlRolesDiscardUnverifiedCompletion(t *testing.T) {
	p, _, r := setupRoles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, lose := range []bool{false, true} {
		fault := &completionFault{loseAfterCommit: lose}
		cfg, err := pgxpool.ParseConfig(storetest.DSN)
		if err != nil {
			t.Fatal("fault config failed")
		}
		cfg.MaxConns = 1
		cfg.ConnConfig.Tracer = fault
		actor, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("fault pool failed")
		}
		var before uint32
		if err := actor.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&before); err != nil {
			t.Fatal("initial backend unavailable")
		}
		_, err = ApplyRoles(ctx, actor, r, Passwords{Export: "synthetic-only-" + strings.Repeat("S", 32)})
		if err == nil || !strings.Contains(err.Error(), "roles committed;") {
			t.Error("postcommit cleanup uncertainty did not preserve confirmed outcome")
		}
		var after uint32
		if err := actor.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&after); err != nil || after == before {
			t.Error("uncertain or unsafe connection returned to pool")
		}
		actor.Close()
	}
	// The owner connection can still verify and remove the managed test roles.
	if problems, _, err := VerifyRoles(ctx, p, r); err != nil || len(problems) != 0 {
		t.Error("committed roles are not intact")
	}
}
