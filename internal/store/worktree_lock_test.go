package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// goodqa#15283 red-team K7: 0038 sets its own lock_timeout. While another
// session holds a lock on attempts, the migration fails fast with 55P03
// instead of waiting (and queueing every other attempts query behind its
// ACCESS EXCLUSIVE request); once the holder is gone it applies.
func TestMigrate0038LockTimeout(t *testing.T) {
	st, ctx, _ := freshStore(t, migration0037)
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background()) //nolint:errcheck
	if _, err := holder.Exec(ctx, "LOCK TABLE attempts IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	began := time.Now()
	err = st.Migrate(mctx)
	took := time.Since(began)
	if pgCode(err) != "55P03" || took > 15*time.Second {
		t.Fatalf("migrate under a held attempts lock: %v after %s, want SQLSTATE 55P03 (lock_timeout) well before 20s", err, took.Round(time.Millisecond))
	}
	if _, latest := level(t, st, ctx); latest != migration0037 {
		t.Fatalf("a timed-out 0038 left level %s, want %s", latest, migration0037)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate once the lock is released: %v", err)
	}
	// Later migrations (0039 and on) apply too: 0038 must be applied, and the
	// level is the latest embedded migration.
	if _, latest := level(t, st, ctx); latest != lastEmbedded(t) || !applied(t, st, ctx, migration0038) {
		t.Fatalf("level %s after retry, want %s with 0038 applied", latest, lastEmbedded(t))
	}
	if !strings.Contains(mustRead(t, migration0038), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("0038 does not set its lock_timeout")
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := migrationFS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lastEmbedded is the binary's newest migration name.
func lastEmbedded(t *testing.T) string {
	t.Helper()
	names, err := embeddedMigrations()
	if err != nil || len(names) == 0 {
		t.Fatal(err)
	}
	return names[len(names)-1]
}

// afterCount is how many embedded migrations sort after name.
func afterCount(t *testing.T, name string) int {
	t.Helper()
	names, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range names {
		if x > name {
			n++
		}
	}
	return n
}

func applied(t *testing.T, st *Store, ctx context.Context, name string) bool {
	t.Helper()
	return count(t, st, ctx, "SELECT count(*) FROM fabric_schema WHERE name = '"+name+"'") == 1
}
