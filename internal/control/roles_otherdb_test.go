package control

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// failDatabaseConnect simulates a database becoming unreachable after catalog
// discovery, using a cancelled real connection attempt rather than fake rows.
type failDatabaseConnect struct{ database string }

func (f failDatabaseConnect) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (f failDatabaseConnect) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (f failDatabaseConnect) TraceConnectStart(ctx context.Context, data pgx.TraceConnectStartData) context.Context {
	if data.ConnConfig.Database == f.database {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled
	}
	return ctx
}
func (f failDatabaseConnect) TraceConnectEnd(context.Context, pgx.TraceConnectEndData) {}

// goodqa#15623 AC-14 (code part): the other-database check connects only to the
// allowlist (fabric_live, rehearsal copies, scratch databases), finds a CREATE right on
// schema public there, and records every other database as unchecked and never
// connected to.
func TestControlRolesOtherDatabases(t *testing.T) {
	p, ctx, r := setupRoles(t)
	var n int
	if err := p.QueryRow(ctx, "SELECT (random() * 900000)::int + 100000").Scan(&n); err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("fabtest_%d", n)
	other := fmt.Sprintf("ctt_other_%d", n)
	for _, db := range []string{scratch, other} {
		if _, err := p.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		db := db
		t.Cleanup(func() { p.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)") }) //nolint:errcheck
	}
	problems, notes, err := VerifyRoles(ctx, p, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("clean state: problems %v", problems)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "database "+scratch+": checked") {
		t.Errorf("the allowlisted scratch database %s was not checked:\n%s", scratch, joined)
	}
	if strings.Contains(joined, "database "+other+": checked") || !strings.Contains(joined, "unchecked, not connected") {
		t.Errorf("a database outside the allowlist must be unchecked and never connected:\n%s", joined)
	}
	// a CREATE right on schema public in the scratch database is a problem
	cfg := p.Config().ConnConfig.Copy()
	cfg.Database = scratch
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	if _, err := conn.Exec(ctx, "GRANT CREATE ON SCHEMA public TO "+quoteID(r.Ingest())); err != nil {
		t.Fatal(err)
	}
	problems, _, err = VerifyRoles(ctx, p, r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pr := range problems {
		if strings.Contains(pr, "may CREATE in schema public of database "+scratch) {
			found = true
		}
	}
	if !found {
		t.Errorf("a CREATE grant on schema public in %s was not reported: %v", scratch, problems)
	}
	if _, err := conn.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM "+quoteID(r.Ingest())); err != nil {
		t.Fatal(err)
	}
	// Legacy or application schemas can grant CREATE independently of public.
	// Probe both a direct grant and PUBLIC; neither is safe for a control role.
	if _, err := conn.Exec(ctx, "CREATE SCHEMA application_data"); err != nil {
		t.Fatal(err)
	}
	for _, grantee := range []string{quoteID(r.Ingest()), "PUBLIC"} {
		t.Run("application-schema-"+grantee, func(t *testing.T) {
			if _, err := conn.Exec(ctx, "GRANT CREATE ON SCHEMA application_data TO "+grantee); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := conn.Exec(ctx, "REVOKE CREATE ON SCHEMA application_data FROM "+grantee); err != nil {
					t.Error(err)
				}
			})
			problems, _, err := VerifyRoles(ctx, p, r)
			if err != nil {
				t.Fatal(err)
			}
			want := "role " + r.Ingest() + " may CREATE in schema application_data of database " + scratch
			for _, problem := range problems {
				if problem == want {
					return
				}
			}
			t.Errorf("CREATE through %s went unreported: %v", grantee, problems)
		})
	}
	t.Run("unreachable-allowlisted-database", func(t *testing.T) {
		cfg := p.Config()
		cfg.ConnConfig.Tracer = failDatabaseConnect{database: scratch}
		probe, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Close()
		problems, _, err := VerifyRoles(ctx, probe, r)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range problems {
			if strings.Contains(problem, "database "+scratch+" could not be checked") {
				return
			}
		}
		t.Errorf("unreachable allowlisted database was reported clean: %v", problems)
	})
}
