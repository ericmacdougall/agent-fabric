package control

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

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
}
