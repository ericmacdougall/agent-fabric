package ingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/control"
	"fabric/internal/storetest"
)

// goodqa#15623 L6 of the roles code review: the ingest runs as the ingest role,
// which has INSERT on named columns and no SELECT. A conflict target in ON CONFLICT
// needs SELECT on its columns (SQLSTATE 42501), so the insert must use ON CONFLICT
// DO NOTHING without a target. The test runs the whole ingest as that role.
func TestIngestAsIngestRole(t *testing.T) {
	st := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := control.Bootstrap(ctx, st.Pool()); err != nil {
		t.Fatal(err)
	}
	var suffix string
	if err := st.Pool().QueryRow(ctx, "SELECT substr(md5(random()::text || clock_timestamp()::text), 1, 8)").Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	r := control.Roles{Prefix: "ctt_" + suffix}
	if _, err := control.ApplyRoles(ctx, st.Pool(), r, control.Passwords{}); err != nil {
		t.Fatalf("ApplyRoles: %v", err)
	}
	t.Cleanup(func() { control.RemoveRoles(context.Background(), st.Pool(), r, true) }) //nolint:errcheck
	c, err := pgx.Connect(ctx, storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) }) //nolint:errcheck
	if _, err := c.Exec(ctx, fmt.Sprintf(`SET ROLE "%s"`, r.Ingest())); err != nil {
		t.Fatal(err)
	}
	res, stderr, err := run(t, ctx, c, ev(1)+ev(2))
	if err != nil {
		t.Fatalf("ingest as the ingest role: %v (%s)", err, stderr)
	}
	if res.Stored.Inserted != 2 {
		t.Fatalf("inserted %d as the ingest role, want 2 (%s)", res.Stored.Inserted, stderr)
	}
	res, stderr, err = run(t, ctx, c, ev(1)+ev(2)+ev(3))
	if err != nil {
		t.Fatalf("replay as the ingest role: %v (%s)", err, stderr)
	}
	if res.Stored.Inserted != 1 || res.Stored.Existing != 2 {
		t.Fatalf("replay: inserted %d existing %d, want 1 and 2 (%s)", res.Stored.Inserted, res.Stored.Existing, stderr)
	}
	if _, err := c.Exec(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if n := ingested(t, ctx, c); n != 3 {
		t.Fatalf("%d rows stored, want 3", n)
	}
}
