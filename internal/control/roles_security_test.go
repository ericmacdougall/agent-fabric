package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		if _, err := p.Exec(ctx, `INSERT INTO missions(id,ticket,state,provider) VALUES($1,'GoodQA goodqa#15623','{}','claude-code');`, mission); err != nil {
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

// Sensitive synchronization uses backend PID/locks, never query/argument tracers.
func TestControlRolePasswordActivityDuringBlockedAlter(t *testing.T) {
	p, _, r := setupRoles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	marker := "synthetic-only-" + strings.Repeat("Q", 32)
	var super bool
	if err := p.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&super); err != nil || !super {
		t.Fatal("observer requires superuser stats visibility")
	}
	for _, protected := range []bool{false, true} {
		locker, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal("locker unavailable")
		}
		tx, err := locker.Begin(ctx)
		if err != nil {
			t.Fatal("locker transaction unavailable")
		}
		var oid uint32
		if err := tx.QueryRow(ctx, "SELECT oid FROM pg_authid WHERE rolname=$1 FOR UPDATE", r.Export()).Scan(&oid); err != nil {
			t.Fatal("owned role lock unavailable")
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			tx.Rollback(ctx)
			locker.Release()
		})
		blocker := locker.Conn().PgConn().PID()
		cfg, err := pgxpool.ParseConfig(storetest.DSN)
		if err != nil {
			t.Fatal("actor config")
		}
		app := r.Prefix + "_password_observer"
		cfg.ConnConfig.RuntimeParams["application_name"] = app
		cfg.MaxConns = 1
		actor, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("actor pool")
		}
		defer actor.Close()
		result := make(chan error, 1)
		go func() {
			if protected {
				_, err := ApplyRoles(ctx, actor, r, Passwords{Export: marker})
				result <- err
				return
			}
			c, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
			if err != nil {
				result <- errors.New("baseline connection failed")
				return
			}
			defer c.Close(ctx)
			btx, err := c.Begin(ctx)
			if err != nil {
				result <- errors.New("baseline transaction failed")
				return
			}
			defer btx.Rollback(ctx)
			for _, q := range []string{"SET LOCAL track_activities=on", "SET LOCAL log_min_error_statement=panic", "SET LOCAL log_statement=none", "SET LOCAL log_min_duration_statement=-1", "SET LOCAL pg_stat_statements.track_utility=off"} {
				if _, err := btx.Exec(ctx, q); err != nil {
					result <- errors.New("baseline protection failed")
					return
				}
			}
			escaped, err := c.PgConn().EscapeString(marker)
			if err != nil {
				result <- errors.New("baseline fixture escaping failed")
				return
			}
			if _, err := btx.Exec(ctx, "ALTER ROLE "+quoteID(r.Export())+" PASSWORD '"+escaped+"'"); err != nil {
				result <- errors.New("baseline ALTER failed")
				return
			}
			result <- btx.Commit(ctx)
		}()
		var pid uint32
		blocked := false
		for ctx.Err() == nil {
			if err := p.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE application_name=$1 AND $2::int=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, app, blocker).Scan(&pid); err != nil {
				t.Fatal("blocking observer failed")
			}
			if pid != 0 {
				blocked = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("intended ALTER not proven blocked on owned role")
		}
		p.Exec(ctx, "SELECT pg_stat_clear_snapshot()")
		var matches int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND strpos(query,$2)>0", pid, marker).Scan(&matches); err != nil {
			t.Fatal("activity count failed")
		}
		t.Logf("protected=%t matching_activity_count=%d", protected, matches)
		if (!protected && matches == 0) || (protected && matches != 0) {
			t.Error("activity visibility control failed")
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal("owned lock release failed")
		}
		locker.Release()
		select {
		case err := <-result:
			if err != nil {
				t.Error("actor operation failed; raw detail suppressed")
			}
		case <-ctx.Done():
			t.Fatal("actor did not finish")
		}
		var remaining int
		for ctx.Err() == nil {
			if err := p.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid=$1", pid).Scan(&remaining); err != nil {
				t.Fatal("backend-close observer failed")
			}
			if remaining == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if remaining != 0 {
			t.Error("dedicated backend remained after operation")
		}
	}
}

func TestControlRolesRefuseUnprotectedActivity(t *testing.T) {
	p, ctx, r := setupRoles(t)
	admin := r.Prefix + "_limited"
	if _, err := p.Exec(ctx, "CREATE ROLE "+quoteID(admin)+" NOLOGIN CREATEROLE; GRANT USAGE ON SCHEMA control TO "+quoteID(admin)+"; GRANT SELECT ON control.ingested_events TO "+quoteID(admin)); err != nil {
		t.Fatal("limited actor setup")
	}
	t.Cleanup(func() {
		p.Exec(context.Background(), "REVOKE ALL ON control.ingested_events FROM "+quoteID(admin)+"; REVOKE ALL ON SCHEMA control FROM "+quoteID(admin)+"; DROP ROLE "+quoteID(admin))
	})
	cfg, err := pgxpool.ParseConfig(storetest.DSN)
	if err != nil {
		t.Fatal("limited config")
	}
	cfg.ConnConfig.RuntimeParams["role"] = admin
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("limited pool")
	}
	defer limited.Close()
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal("snapshot connection")
	}
	before, err := rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil {
		t.Fatal("snapshot failed")
	}
	_, err = ApplyRoles(ctx, limited, r, Passwords{Export: "synthetic-only-" + strings.Repeat("R", 32)})
	if err == nil {
		t.Error("unprotected activity path accepted")
	}
	c, err = p.Acquire(ctx)
	if err != nil {
		t.Fatal("readback connection")
	}
	after, err := rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil || before != after {
		t.Error("refusal changed role state")
	}
}

// An observing fault tracer is now explicitly unsupported before transmission.
type completionFault struct{}

func (completionFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (completionFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestControlRolesDiscardUnverifiedCompletion(t *testing.T) {
	p, ctx, r := setupRoles(t)
	cfg := p.Config()
	cfg.ConnConfig.Tracer = completionFault{}
	actor, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("observing actor config")
	}
	defer actor.Close()
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal("snapshot connection")
	}
	before, err := rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil {
		t.Fatal("snapshot failed")
	}
	if _, err := ApplyRoles(ctx, actor, r, Passwords{Export: "synthetic-only-" + strings.Repeat("S", 32)}); err == nil {
		t.Error("observing callback was silently bypassed")
	}
	c, err = p.Acquire(ctx)
	if err != nil {
		t.Fatal("readback connection")
	}
	after, err := rolesDigest(ctx, c.Conn(), r)
	c.Release()
	if err != nil || after != before {
		t.Error("unsupported callback changed roles")
	}
	if problems, _, err := VerifyRoles(ctx, p, r); err != nil || len(problems) != 0 {
		t.Error("refused operation damaged managed roles")
	}
}
