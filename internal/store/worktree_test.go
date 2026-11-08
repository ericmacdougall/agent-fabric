package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/domain"
)

const migration0037 = "migrations/0037_review_gate_call_once.sql"
const migration0038 = "migrations/0038_attempt_worktree.sql"

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// goodqa#15283 AC-4, NEG-5: the database enforces the worktree's validity and
// immutability on its own. One table of vectors runs through both
// domain.ValidWorktree and attempts_worktree_check and must agree; go-only
// vectors are refused by Go and need not be by the CHECK.
func TestAttemptsWorktreeConstraints(t *testing.T) {
	st, ctx := openTest(t)
	mustExec(t, st, ctx, `INSERT INTO missions (id, ticket, state, provider) VALUES
		('m_wt_api', 'goodqa#15283', '{}', 'anthropic-api'), ('m_wt_cc', 'goodqa#15283', '{}', 'claude-code')`)
	fence := 0
	insert := func(id, path string, wt *string) error {
		fence++ // attempts are unique on (mission, fence)
		mission, lock := "m_wt_api", "/tmp/native-locks/"+id+".lock"
		var lockArg any = lock
		if path == "claude-code" {
			mission, lockArg = "m_wt_cc", nil
		}
		_, err := st.pool.Exec(ctx, `INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path, lock_path, worktree)
			VALUES ($1, $2, $6, 0, 0, 'ENDED', $3, $4, $5)`, id, mission, path, lockArg, wt, fence)
		return err
	}
	long := "/" + strings.Repeat("a", domain.MaxWorktreeBytes-1)
	for i, v := range []struct {
		what   string
		path   string
		wt     string
		ok     bool
		goOnly bool // Go refuses; the CHECK need not
	}{
		{"absolute", "anthropic-api", "/srv/wt", true, false},
		{"upper case kept", "anthropic-api", "/srv/WT", true, false},
		{"space and non-ASCII letter", "anthropic-api", "/srv/a b/\u00e9", true, false},
		{"4096 bytes", "anthropic-api", long, true, false},
		{"empty", "anthropic-api", "", false, false},
		{"relative", "anthropic-api", "srv/wt", false, false},
		{"root", "anthropic-api", "/", false, false},
		{"dot-dot segment", "anthropic-api", "/srv/../wt", false, false},
		{"trailing dot-dot", "anthropic-api", "/srv/..", false, false},
		{"dot segment", "anthropic-api", "/srv/./wt", false, false},
		{"double slash", "anthropic-api", "/srv//wt", false, false},
		{"trailing slash", "anthropic-api", "/srv/wt/", false, false},
		{"NUL", "anthropic-api", "/srv/w\x00t", false, false},
		{"C0 control", "anthropic-api", "/srv/w\x01t", false, false},
		{"newline", "anthropic-api", "/srv/w\nt", false, false},
		{"unit separator", "anthropic-api", "/srv/w\x1ft", false, false},
		{"DEL", "anthropic-api", "/srv/w\x7ft", false, false},
		{"C1 control", "anthropic-api", "/srv/w\u0085t", false, false},
		{"4097 bytes", "anthropic-api", long + "a", false, false},
		{"claude-code row with a worktree", "claude-code", "/srv/wt", false, false},
		{"invalid UTF-8", "anthropic-api", "/srv/\xffwt", false, true},
		{"zero-width space (Cf)", "anthropic-api", "/srv/w\u200bt", false, true},
		{"right-to-left override (bidi)", "anthropic-api", "/srv/w\u202et", false, true},
		{"left-to-right isolate (bidi)", "anthropic-api", "/srv/w\u2066t", false, true},
	} {
		id := "r_wtv" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		wt := v.wt
		dbErr := insert(id, v.path, &wt)
		dbOK := dbErr == nil
		// The Go half: the path rule, and the reducer's provider rule.
		goErr := domain.ValidWorktree(v.wt)
		if v.path == "claude-code" {
			goErr = errors.New("a claude-code attempt has no worktree")
		}
		goOK := goErr == nil
		if goOK != v.ok {
			t.Errorf("%s: ValidWorktree(%q) = %v, want ok=%v", v.what, v.wt, goErr, v.ok)
		}
		if v.goOnly {
			t.Logf("%s (go-only): CHECK ok=%v (%v)", v.what, dbOK, dbErr)
			continue
		}
		if dbOK != v.ok {
			t.Errorf("%s: CHECK accepted=%v (%v), want %v", v.what, dbOK, dbErr, v.ok)
		}
		if dbOK != goOK {
			t.Errorf("%s: Go and the database disagree: Go %v, database %v", v.what, goErr, dbErr)
		}
		// A structural refusal must come from the named constraint; NUL never
		// reaches it (Postgres text cannot hold one: 22021).
		if !v.ok && v.what != "NUL" {
			var pe *pgconn.PgError
			if !errors.As(dbErr, &pe) || pe.Code != "23514" || pe.ConstraintName != "attempts_worktree_check" {
				t.Errorf("%s: refusal %v, want 23514 from attempts_worktree_check", v.what, dbErr)
			}
		}
	}
	if err := insert("r_wtnull", "anthropic-api", nil); err != nil {
		t.Fatalf("NULL worktree refused: %v", err)
	}
	if err := insert("r_wtccnull", "claude-code", nil); err != nil {
		t.Fatalf("claude-code row with no worktree refused: %v", err)
	}

	// Immutability: a plain BEFORE UPDATE trigger, so every UPDATE that
	// changes the column is refused, whatever else the statement names.
	a := "/srv/a"
	if err := insert("r_wtimm", "anthropic-api", &a); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ what, id, sql string }{
		{"NULL to valid", "r_wtnull", `UPDATE attempts SET worktree = '/srv/b' WHERE id = 'r_wtnull'`},
		{"valid A to valid B", "r_wtimm", `UPDATE attempts SET worktree = '/srv/b' WHERE id = 'r_wtimm'`},
		{"valid to NULL", "r_wtimm", `UPDATE attempts SET worktree = NULL WHERE id = 'r_wtimm'`},
		{"with another column", "r_wtimm", `UPDATE attempts SET state = 'ENDED', worktree = '/srv/b' WHERE id = 'r_wtimm'`},
	} {
		_, err := st.pool.Exec(ctx, c.sql)
		if pgCode(err) != "23001" || !strings.Contains(err.Error(), "attempt "+c.id+" worktree is immutable") {
			t.Errorf("%s: %v, want SQLSTATE 23001 and the immutability message", c.what, err)
		}
	}
	// Controls: an UPDATE that leaves the worktree as it is passes.
	for _, q := range []string{
		`UPDATE attempts SET state = 'ENDED' WHERE id = 'r_wtimm'`,
		`UPDATE attempts SET worktree = '/srv/a' WHERE id = 'r_wtimm'`,
		`UPDATE attempts SET worktree = NULL WHERE id = 'r_wtnull'`,
	} {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Errorf("%s: %v, want allowed", q, err)
		}
	}
	var got *string
	if err := st.pool.QueryRow(ctx, `SELECT worktree FROM attempts WHERE id = 'r_wtimm'`).Scan(&got); err != nil || got == nil || *got != a {
		t.Fatalf("worktree after refused updates: %v %v, want %q", got, err, a)
	}
}

// goodqa#15283 AC-6: 0038 applies forward from level 37, existing rows keep
// a NULL worktree, and applying it again is a no-op.
func TestMigrate0038FromLevel37(t *testing.T) {
	// "Level 37" is the latest applied name; the row count is lower because
	// 0027, 0028 and 0030 were never shipped.
	st, ctx, _ := freshStore(t, migration0037)
	n37, latest := level(t, st, ctx)
	if latest != migration0037 {
		t.Fatalf("setup: level %d at %s, want %s", n37, latest, migration0037)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'attempts' AND column_name = 'worktree'`); n != 0 {
		t.Fatal("setup: level 37 already has attempts.worktree")
	}
	mustExec(t, st, ctx,
		`INSERT INTO missions (id, ticket, state, provider) VALUES
			('m_l37_api', 'goodqa#15283', '{}', 'anthropic-api'), ('m_l37_cc', 'goodqa#15283', '{}', 'claude-code')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path, lock_path) VALUES
			('r_l37_api', 'm_l37_api', 1, 0, 0, 'RUNNING', 'anthropic-api', '/tmp/native-locks/r_l37_api.lock'),
			('r_l37_cc', 'm_l37_cc', 1, 0, 0, 'RUNNING', 'claude-code', NULL)`)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate 37 -> 38: %v", err)
	}
	if n, latest := level(t, st, ctx); n != n37+1 || latest != migration0038 {
		t.Fatalf("level %d at %s, want %d at %s", n, latest, n37+1, migration0038)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM attempts`); n != 2 {
		t.Fatalf("%d attempts after migrate, want 2", n)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM attempts WHERE worktree IS NULL`); n != 2 {
		t.Fatalf("%d attempts with a NULL worktree, want both", n)
	}
	// A second Migrate is a no-op, and the file itself re-applies cleanly.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	sql, err := migrationFS.ReadFile(migration0038)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-applying 0038: %v", err)
	}
	if n, _ := level(t, st, ctx); n != n37+1 {
		t.Fatalf("level %d after re-apply, want %d", n, n37+1)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'attempts_worktree_immutable'`); n != 1 {
		t.Fatalf("%d immutability triggers after re-apply, want 1", n)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'attempts_worktree_check'`); n != 1 {
		t.Fatalf("%d worktree checks after re-apply, want 1", n)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM attempts WHERE worktree IS NULL`); n != 2 {
		t.Fatalf("re-apply touched rows: %d NULL, want 2", n)
	}
}

// goodqa#15283 AC-7: a binary already running during the migration, or the
// cf50e55 binary after it, inserts an attempt without the worktree column.
// That INSERT (the cf50e55 statement, verbatim) still succeeds on 0038 and
// stores NULL.
func TestAttemptInsertWithoutWorktreeOn0038(t *testing.T) {
	st, ctx := openTest(t)
	mustExec(t, st, ctx, `INSERT INTO missions (id, ticket, state, provider) VALUES ('m_old', 'goodqa#15283', '{}', 'anthropic-api')`)
	_, err := st.pool.Exec(ctx, `INSERT INTO attempts (id, mission_id, fence, revision, generation, host, role, state, lease_until, path, lock_path)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + $9::interval, $10, NULLIF($11, ''))`,
		"r_old", "m_old", int64(1), int64(0), int64(0), "h_t2", "writer", string(domain.AttemptLeased), "5 minutes", "anthropic-api",
		"/tmp/native-locks/r_old.lock")
	if err != nil {
		t.Fatalf("pre-0038 INSERT refused on 0038: %v", err)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM attempts WHERE id = 'r_old' AND worktree IS NULL`); n != 1 {
		t.Fatal("pre-0038 INSERT did not store a NULL worktree")
	}
}
