package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func markerValue(t *testing.T, ctx context.Context, st *Store) (string, bool) {
	t.Helper()
	var v string
	err := st.pool.QueryRow(ctx, `SELECT value FROM fabric_settings WHERE key = 'comment_policy'`).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// goodqa#15966 M1 comment policy: `fab db roles --comment-policy m1` writes
// the marker in the roles transaction; plain `fab db roles` leaves it alone;
// an injected failure later in the transaction leaves no marker; the app and
// runner roles cannot write it.
func TestApplyRolesCommentPolicy(t *testing.T) {
	owner, ctx := openTest(t)
	owner.pool.Exec(ctx, `DELETE FROM fabric_settings WHERE key = 'comment_policy'`) //nolint:errcheck
	t.Cleanup(func() {
		owner.pool.Exec(context.Background(), `DELETE FROM fabric_settings WHERE key = 'comment_policy'`) //nolint:errcheck
	})
	r := Roles{Prefix: fmt.Sprintf("fabric_cp%d", os.Getpid())}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			owner.pool.Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			owner.pool.Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})

	// All or nothing: a fault after the marker statement leaves no marker.
	applyRolesFault = func(stmt string) error {
		if strings.Contains(stmt, "comment_policy") {
			return nil
		}
		if strings.HasPrefix(stmt, "REVOKE CREATE ON SCHEMA public FROM PUBLIC") {
			return errors.New("injected")
		}
		return nil
	}
	if err := owner.ApplyRolesPolicy(ctx, r, "", "", CommentPolicyM1); err == nil {
		t.Fatal("injected fault not reported")
	}
	applyRolesFault = nil
	if _, ok := markerValue(t, ctx, owner); ok {
		t.Fatal("a failed roles run left the marker")
	}

	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := markerValue(t, ctx, owner); ok {
		t.Fatal("plain roles wrote the marker")
	}
	if err := owner.ApplyRolesPolicy(ctx, r, "", "", CommentPolicyM1); err != nil {
		t.Fatal(err)
	}
	if v, ok := markerValue(t, ctx, owner); !ok || v != "m1" {
		t.Fatalf("marker %q %v", v, ok)
	}
	// Plain roles afterwards (any build) leaves the marker in place, and the
	// scheduler_role reader still sees only its key.
	if err := owner.ApplyRoles(ctx, r, "", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok := markerValue(t, ctx, owner); !ok || v != "m1" {
		t.Fatalf("plain roles changed the marker: %q %v", v, ok)
	}
	if err := owner.ApplyRolesPolicy(ctx, r, "", "", "m2"); err == nil {
		t.Fatal("accepted an unknown policy value")
	}
	app := openAs(t, ctx, r.App())
	for _, q := range []string{`DELETE FROM fabric_settings WHERE key = 'comment_policy'`, `UPDATE fabric_settings SET value = 'x' WHERE key = 'comment_policy'`} {
		if _, err := app.pool.Exec(ctx, q); err == nil {
			t.Fatalf("app role ran %s", q)
		}
	}
	var v string
	if err := app.pool.QueryRow(ctx, `SELECT value FROM fabric_settings WHERE key = 'comment_policy'`).Scan(&v); err != nil || v != "m1" {
		t.Fatalf("app role cannot read the marker: %q %v", v, err)
	}
}
