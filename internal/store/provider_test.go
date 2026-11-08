package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/domain"
)

// goodqa#15087 increment 1: missions.provider, attempts.path and
// attempts.lock_path (migration 0026), the forward guard, and their
// refusals.

const (
	level25 = "migrations/0025_runner_job_insert_guard.sql"
	level26 = "migrations/0026_mission_provider.sql"
)

var freshSeq atomic.Int64

// freshStore creates a throwaway database migrated up to upTo (every
// migration when upTo is empty) and drops it when the test ends. The
// shared per-process database cannot go back to an older schema.
func freshStore(t *testing.T, upTo string) (*Store, context.Context, string) {
	t.Helper()
	admin := os.Getenv("FABRIC_TEST_DSN")
	if admin == "" {
		t.Fatal("FABRIC_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	ap, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("fabric_t_%d_f%d", os.Getpid(), freshSeq.Add(1))
	if _, err := ap.Exec(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		ap.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ap.Exec(context.Background(), "DROP DATABASE "+quoteIdent(name)+" WITH (FORCE)") //nolint:errcheck
		ap.Close()
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal("admin dsn does not parse")
	}
	u.Path = "/" + name
	dsn := u.String()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.migrateUpTo(ctx, upTo); err != nil {
		t.Fatalf("migrate up to %q: %v", upTo, err)
	}
	return st, ctx, dsn
}

func mustExec(t *testing.T, st *Store, ctx context.Context, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rawCommand is a schema-25 command row: canonical bytes "{}", so the 0011
// checks hold without the store's encoder.
func rawCommand(actor, mission, id, kind string) string {
	return fmt.Sprintf(`INSERT INTO commands (actor, mission_id, command_id, kind, request_sha256, status, response, response_bytes, canonical_request)
		VALUES ('%s', '%s', '%s', '%s', encode(sha256('\x7b7d'::bytea), 'hex'), 'APPLIED', '{}'::jsonb, '\x7b7d'::bytea, '\x7b7d'::bytea)`,
		actor, mission, id, kind)
}

// seed25 writes the AC-8 rows into a schema-25 database.
func seed25(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	mustExec(t, st, ctx,
		`INSERT INTO missions (id, ticket, state) VALUES
			('m_nat1', 'goodqa#1', '{}'), ('m_nat2', 'goodqa#2', '{}'), ('m_cc', 'goodqa#3', '{}'), ('m_none', 'goodqa#4', '{}')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state) VALUES
			('r_nat1', 'm_nat1', 1, 0, 0, 'RUNNING'), ('r_nat2', 'm_nat2', 1, 0, 0, 'ENDED'), ('r_cc', 'm_cc', 1, 0, 0, 'ENDED')`,
		// m_nat1: native by its agent_runs row alone (no commands at all).
		`INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode) VALUES ('r_nat1', 1, 'x', 'api_key')`,
		// m_nat2: native by its commands alone (no agent_runs row).
		rawCommand("native-runner", "m_nat2", "native-x-admit", "ADMIT_MISSION"),
		rawCommand("native-runner", "m_nat2", "native-x-start", "START_ATTEMPT"),
		// m_cc: started by fabd.
		rawCommand("chat", "m_cc", "admit", "ADMIT_MISSION"),
		rawCommand("fabd", "m_cc", "start:r_cc", "START_ATTEMPT"),
	)
}

// level reports the migration rows: their count and the latest name.
func level(t *testing.T, st *Store, ctx context.Context) (int, string) {
	t.Helper()
	var n int
	var latest string
	if err := st.pool.QueryRow(ctx, "SELECT count(*), coalesce(max(name), '') FROM fabric_schema").Scan(&n, &latest); err != nil {
		t.Fatal(err)
	}
	return n, latest
}

func columns0026(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	return count(t, st, ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND ((table_name = 'missions' AND column_name = 'provider') OR (table_name = 'attempts' AND column_name IN ('path', 'lock_path')))`)
}

func objects0026(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	return count(t, st, ctx, `SELECT (SELECT count(*) FROM pg_proc WHERE proname IN ('fabric_missions_guard', 'fabric_missions_no_truncate', 'fabric_attempts_path'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgname IN ('missions_guard', 'missions_no_truncate', 'attempts_path'))`)
}

// assertAt25 is the clean-rollback check: nothing of 0026 is left.
func assertAt25(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	if _, latest := level(t, st, ctx); latest != level25 {
		t.Fatalf("level after the failed migrate: %s, want %s", latest, level25)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM fabric_schema WHERE name = $1", level26); n != 0 {
		t.Fatalf("%d 0026 rows in fabric_schema after a failed migrate", n)
	}
	if n := columns0026(t, st, ctx); n != 0 {
		t.Fatalf("%d 0026 columns left after a failed migrate", n)
	}
	if n := objects0026(t, st, ctx); n != 0 {
		t.Fatalf("%d 0026 functions or triggers left after a failed migrate", n)
	}
}

// AC-8: the backfill classifies each kind of mission, and every attempt's
// path is its mission's provider.
func TestMigrate0026Backfill(t *testing.T) {
	st, ctx, _ := freshStore(t, level25)
	seed25(t, st, ctx)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := map[string]domain.Provider{"m_nat1": domain.ProviderAnthropicAPI, "m_nat2": domain.ProviderAnthropicAPI,
		"m_cc": domain.ProviderClaudeCode, "m_none": domain.ProviderClaudeCode}
	for id, p := range want {
		var got string
		if err := st.pool.QueryRow(ctx, "SELECT provider FROM missions WHERE id = $1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if domain.Provider(got) != p {
			t.Errorf("mission %s backfilled as %s, want %s", id, got, p)
		}
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM attempts"); n != 3 {
		t.Fatalf("attempts after migrate: %d, want the 3 seeded", n)
	}
	if n := count(t, st, ctx, `SELECT count(*) FROM attempts a JOIN missions m ON m.id = a.mission_id WHERE a.path IS DISTINCT FROM m.provider`); n != 0 {
		t.Fatalf("%d attempts whose path differs from their mission's provider", n)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM attempts WHERE lock_path IS NOT NULL"); n != 0 {
		t.Fatalf("%d backfilled attempts carry a lock_path; legacy attempts have none", n)
	}
}

// AC-8: a native mission that another actor also started is not guessed:
// migrate stops, names it, and leaves schema 25 as it was.
func TestMigrate0026Ambiguous(t *testing.T) {
	st, ctx, _ := freshStore(t, level25)
	seed25(t, st, ctx)
	mustExec(t, st, ctx,
		`INSERT INTO missions (id, ticket, state) VALUES ('m_amb', 'goodqa#5', '{}')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state) VALUES ('r_amb1', 'm_amb', 1, 0, 0, 'ENDED'), ('r_amb2', 'm_amb', 2, 0, 0, 'ENDED')`,
		`INSERT INTO agent_runs (attempt_id, fence, config_sha, billing_mode) VALUES ('r_amb1', 1, 'x', 'api_key')`,
		rawCommand("fabd", "m_amb", "start:r_amb2", "START_ATTEMPT"),
	)
	err := st.Migrate(ctx)
	if err == nil {
		t.Fatal("migrate classified an ambiguous mission instead of refusing it")
	}
	if sqlCode(err) != "23514" || !strings.Contains(err.Error(), "m_amb") {
		t.Fatalf("migrate error %q (sqlstate %q), want a check_violation naming m_amb", err, sqlCode(err))
	}
	assertAt25(t, st, ctx)
}

// AC-8: an injected failure inside 0026 rolls all of it back, and a
// re-run after the cause is gone applies it.
func TestMigrate0026InjectedFailure(t *testing.T) {
	st, ctx, _ := freshStore(t, level25)
	seed25(t, st, ctx)
	mustExec(t, st, ctx,
		`CREATE FUNCTION test_0026_inject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected 0026 failure'; END $$`,
		`CREATE TRIGGER test_0026_inject BEFORE UPDATE ON missions FOR EACH STATEMENT EXECUTE FUNCTION test_0026_inject()`,
	)
	err := st.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "injected 0026 failure") {
		t.Fatalf("migrate with the injected trigger: %v, want the injected failure", err)
	}
	assertAt25(t, st, ctx)
	mustExec(t, st, ctx, `DROP TRIGGER test_0026_inject ON missions`, `DROP FUNCTION test_0026_inject()`)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate after removing the injected trigger: %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM fabric_schema WHERE name = $1", level26); n != 1 {
		t.Fatalf("0026 rows after the re-run: %d, want 1", n)
	}
	if n := columns0026(t, st, ctx); n != 3 {
		t.Fatalf("0026 columns after the re-run: %d, want 3", n)
	}
}

// AC-9, NEG-10: a binary refuses a database holding a migration it does
// not embed, naming its own latest migration and the foreign one.
func TestRefuseNewerSchema(t *testing.T) {
	st, ctx, dsn := freshStore(t, "")
	names, err := embeddedMigrations()
	if err != nil || len(names) == 0 {
		t.Fatalf("embedded migrations: %v (%d)", err, len(names))
	}
	latest := names[len(names)-1]
	const foreign = "migrations/0099_future.sql"
	mustExec(t, st, ctx, "INSERT INTO fabric_schema (name, sha256) VALUES ('"+foreign+"', 'x')")
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNewerSchema) {
			t.Fatalf("%s against a newer schema: %v, want ErrNewerSchema", what, err)
		}
		if !strings.Contains(err.Error(), "0099") || !strings.Contains(err.Error(), latest) {
			t.Fatalf("%s error %q must name 0099 and %s", what, err, latest)
		}
	}
	_, err = Open(ctx, dsn)
	check("Open", err)
	check("Migrate", st.Migrate(ctx))
	mustExec(t, st, ctx, "DELETE FROM fabric_schema WHERE name = '"+foreign+"'")
	again, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open after removing the foreign row: %v", err)
	}
	again.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after removing the foreign row: %v", err)
	}
}

// AC-1: every creation path needs a valid provider; the column has no
// default, so a binary that predates it cannot create a mission.
func TestMissionNeedsProvider(t *testing.T) {
	st, ctx := openTest(t)
	for _, p := range []domain.Provider{"", "claude.ai", "Claude-Code", "anthropic-api "} {
		if err := st.CreateMission(ctx, "m_np", p, "GoodQA goodqa#15087", "", ""); !errors.Is(err, domain.ErrInvalidProvider) {
			t.Errorf("CreateMission with provider %q: %v, want ErrInvalidProvider", p, err)
		}
		if err := st.CreateMissionSpec(ctx, MissionSpec{ID: "m_np", Ticket: "GoodQA goodqa#15087", Provider: p}); !errors.Is(err, domain.ErrInvalidProvider) {
			t.Errorf("CreateMissionSpec with provider %q: %v, want ErrInvalidProvider", p, err)
		}
	}
	_, err := st.pool.Exec(ctx, `INSERT INTO missions (id, ticket, state) VALUES ('m_np', 'goodqa#1', '{}')`)
	if sqlCode(err) != "23502" {
		t.Errorf("insert without a provider: %v, want not_null_violation", err)
	}
	_, err = st.pool.Exec(ctx, `INSERT INTO missions (id, ticket, state, provider) VALUES ('m_np', 'goodqa#1', '{}', 'claude.ai')`)
	if sqlCode(err) != "23514" {
		t.Errorf("insert with provider claude.ai: %v, want check_violation", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM missions WHERE id = 'm_np'"); n != 0 {
		t.Fatalf("a refused creation left %d rows", n)
	}
	for _, p := range []domain.Provider{domain.ProviderClaudeCode, domain.ProviderAnthropicAPI} {
		id := domain.MissionID("m_np_" + strings.ReplaceAll(string(p), "-", "_"))
		if err := st.CreateMission(ctx, id, p, "GoodQA goodqa#15087", "", ""); err != nil {
			t.Fatalf("CreateMission %s: %v", p, err)
		}
		var got string
		if err := st.pool.QueryRow(ctx, "SELECT provider FROM missions WHERE id = $1", string(id)).Scan(&got); err != nil || got != string(p) {
			t.Fatalf("stored provider %q (%v), want %s", got, err, p)
		}
	}
}

// AC-1: re-registering a mission under the other provider is refused in
// both directions; any other difference keeps the first import.
func TestEnsureSpecProviderDiffers(t *testing.T) {
	st, ctx := openTest(t)
	for _, c := range []struct {
		id          domain.MissionID
		first, then domain.Provider
	}{{"m_es1", domain.ProviderClaudeCode, domain.ProviderAnthropicAPI}, {"m_es2", domain.ProviderAnthropicAPI, domain.ProviderClaudeCode}} {
		spec := MissionSpec{ID: c.id, Name: "first", Ticket: "GoodQA goodqa#15087", Priority: 5, Provider: c.first}
		if e, err := st.EnsureMissionSpec(ctx, spec); err != nil || e != Created {
			t.Fatalf("%s first import: %v %v", c.id, e, err)
		}
		other := spec
		other.Provider = c.then
		if _, err := st.EnsureMissionSpec(ctx, other); !errors.Is(err, domain.ErrInvalidProvider) {
			t.Fatalf("%s re-import as %s: %v, want ErrInvalidProvider", c.id, c.then, err)
		}
		renamed := spec
		renamed.Name = "second"
		if e, err := st.EnsureMissionSpec(ctx, renamed); err != nil || e != Differs {
			t.Fatalf("%s re-import with another name: %v %v, want Differs", c.id, e, err)
		}
		if e, err := st.EnsureMissionSpec(ctx, spec); err != nil || e != Unchanged {
			t.Fatalf("%s identical re-import: %v %v, want Unchanged", c.id, e, err)
		}
		var name, prov string
		if err := st.pool.QueryRow(ctx, "SELECT name, provider FROM missions WHERE id = $1", string(c.id)).Scan(&name, &prov); err != nil {
			t.Fatal(err)
		}
		if name != "first" || domain.Provider(prov) != c.first {
			t.Fatalf("%s after re-imports: name %q provider %s, want the first import's", c.id, name, prov)
		}
	}
}

// NEG-2: the provider never changes, a mission is never deleted (so it
// cannot be re-created under the other provider), and TRUNCATE needs the
// opt-in, even for the owner; store.Truncate (owner plus opt-in) still
// resets the database.
func TestProviderImmutable(t *testing.T) {
	st, ctx := openTest(t)
	mk(t, st, ctx, "m_pi")
	_, err := st.pool.Exec(ctx, "UPDATE missions SET provider = 'anthropic-api' WHERE id = 'm_pi'")
	if sqlCode(err) != "23001" || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("owner UPDATE of provider: %v, want restrict_violation (immutable)", err)
	}
	_, err = st.pool.Exec(ctx, "DELETE FROM missions WHERE id = 'm_pi'")
	if sqlCode(err) != "23001" || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("owner DELETE of a mission: %v, want restrict_violation from the guard", err)
	}
	if err := st.CreateMission(ctx, "m_pi", domain.ProviderAnthropicAPI, "GoodQA goodqa#15087", "", ""); !errors.Is(err, domain.ErrInvalidProvider) {
		t.Fatalf("re-create under the other provider: %v, want ErrInvalidProvider", err)
	}
	if err := st.CreateMission(ctx, "m_pi", domain.ProviderClaudeCode, "GoodQA goodqa#14791", "fabric", "main"); err != nil {
		t.Fatalf("idempotent re-create under the same provider: %v", err)
	}
	_, err = st.pool.Exec(ctx, "TRUNCATE missions CASCADE")
	if sqlCode(err) != "23001" || !strings.Contains(err.Error(), "TRUNCATE on missions") {
		t.Fatalf("owner TRUNCATE without the opt-in: %v, want restrict_violation from the missions guard", err)
	}
	var prov string
	if err := st.pool.QueryRow(ctx, "SELECT provider FROM missions WHERE id = 'm_pi'").Scan(&prov); err != nil || prov != string(domain.ProviderClaudeCode) {
		t.Fatalf("after the refusals: provider %q (%v), want claude-code", prov, err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("store.Truncate (owner plus opt-in): %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM missions"); n != 0 {
		t.Fatalf("missions after store.Truncate: %d, want 0", n)
	}
}

// NEG-15: the TRUNCATE opt-in counts only for the session user that owns
// the table. The app role logs in as itself, holds TRUNCATE (granted for
// this test, so the trigger and not the privilege refuses), sets the
// opt-in, and is still refused.
func TestTruncateOptInNeedsOwner(t *testing.T) {
	st, ctx, dsn := freshStore(t, "")
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	pw := hex.EncodeToString(buf[:])
	r := Roles{Prefix: fmt.Sprintf("fabric_tr%d", os.Getpid())}
	if err := st.ApplyRoles(ctx, r, pw, ""); err != nil {
		t.Fatalf("roles: %v", err)
	}
	t.Cleanup(func() {
		for _, n := range []string{r.App(), r.Runner(), r.Scheduler()} {
			st.pool.Exec(context.Background(), "DROP OWNED BY "+n) //nolint:errcheck
			st.pool.Exec(context.Background(), "DROP ROLE "+n)     //nolint:errcheck
		}
	})
	mustExec(t, st, ctx, "GRANT TRUNCATE ON ALL TABLES IN SCHEMA public TO "+r.App())
	if err := st.CreateMission(ctx, "m_tr", domain.ProviderClaudeCode, "GoodQA goodqa#15087", "", ""); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("dsn does not parse")
	}
	u.User = url.UserPassword(r.App(), pw)
	app, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal("app login pool failed")
	}
	defer app.Close()
	var who, sess string
	if err := app.QueryRow(ctx, "SELECT current_user, session_user").Scan(&who, &sess); err != nil {
		t.Fatalf("app login: %v", err)
	}
	if who != r.App() || sess != r.App() {
		t.Fatalf("app connection runs as %s (session %s), want its own login %s", who, sess, r.App())
	}
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL fabric.allow_truncate = 'on'"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, "TRUNCATE missions CASCADE")
	tx.Rollback(ctx) //nolint:errcheck
	if sqlCode(err) != "23001" || !strings.Contains(err.Error(), "TRUNCATE on missions") {
		t.Fatalf("app login with the opt-in: %v, want restrict_violation from the missions guard", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM missions"); n != 1 {
		t.Fatalf("missions after the refused TRUNCATE: %d, want 1", n)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("owner with the opt-in (positive control): %v", err)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM missions"); n != 0 {
		t.Fatalf("missions after the owner's TRUNCATE: %d, want 0", n)
	}
}

// AC-2, NEG-3: the database refuses an attempt whose path differs from its
// mission's provider, in both directions, on INSERT and on UPDATE of path
// or mission_id, and checks lock_path. It runs as the app role (fabd's
// role, the scheduler member) and as the owner. The scheduler role itself
// holds no table privileges, so SET ROLE to it alone cannot insert at all.
func TestAttemptPathGuard(t *testing.T) {
	owner, ctx := openTest(t)
	r := testRoles(t, owner, ctx)
	app := openAs(t, ctx, r.App())
	for _, m := range []struct {
		id domain.MissionID
		p  domain.Provider
	}{{"m_gc", domain.ProviderClaudeCode}, {"m_ga", domain.ProviderAnthropicAPI}} {
		if err := owner.CreateMission(ctx, m.id, m.p, "GoodQA goodqa#15087", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	fence := 0 // (mission_id, fence) is unique: every row takes its own
	ins := func(st *Store, id, mission, path string, lock *string) error {
		fence++
		_, err := st.pool.Exec(ctx, `INSERT INTO attempts (id, mission_id, fence, revision, generation, state, path, lock_path)
			VALUES ($1, $2, $5, 0, 0, 'ENDED', $3, $4)`, id, mission, path, lock, fence)
		return err
	}
	str := func(s string) *string { return &s }
	for _, as := range []struct {
		name string
		st   *Store
	}{{"app", app}, {"owner", owner}} {
		a := func(s string) string { return s + "_" + as.name }
		lock := func(id string) *string { return str("/var/lib/fabric/native-locks/" + id + ".lock") }
		refused := []struct {
			what          string
			id, m, path   string
			lockPath      *string
			code, message string
		}{
			{"anthropic-api path on a claude-code mission", a("r_g1"), "m_gc", "anthropic-api", lock(a("r_g1")), "23514", "differs from mission"},
			{"claude-code path on an anthropic-api mission", a("r_g2"), "m_ga", "claude-code", nil, "23514", "differs from mission"},
			{"no path", a("r_g3"), "m_gc", "", nil, "23514", ""},
			{"native attempt with a NULL lock_path", a("r_g4"), "m_ga", "anthropic-api", nil, "23514", "no lock_path"},
			{"lock_path naming another attempt's file", a("r_g5"), "m_ga", "anthropic-api", lock("r_other"), "23514", "attempts_lock_path_check"},
			{"lock_path with a .. segment", a("r_g6"), "m_ga", "anthropic-api", str("/var/lib/fabric/../x/native-locks/" + a("r_g6") + ".lock"), "23514", "attempts_lock_path_check"},
			{"relative lock_path", a("r_g7"), "m_ga", "anthropic-api", str("state/native-locks/" + a("r_g7") + ".lock"), "23514", "attempts_lock_path_check"},
			{"lock_path with an empty segment", a("r_g8"), "m_ga", "anthropic-api", str("/var/lib//native-locks/" + a("r_g8") + ".lock"), "23514", "attempts_lock_path_check"},
			{"claude-code attempt with a lock_path", a("r_g9"), "m_gc", "claude-code", lock(a("r_g9")), "23514", "attempts_lock_path_check"},
		}
		for _, c := range refused {
			err := ins(as.st, c.id, c.m, c.path, c.lockPath)
			if sqlCode(err) != c.code || !strings.Contains(err.Error(), c.message) {
				t.Errorf("%s INSERT %s: %v, want %s %q", as.name, c.what, err, c.code, c.message)
			}
		}
		// Positive controls: each path on its own mission.
		if err := ins(as.st, a("r_gc"), "m_gc", "claude-code", nil); err != nil {
			t.Fatalf("%s INSERT claude-code attempt: %v", as.name, err)
		}
		if err := ins(as.st, a("r_ga"), "m_ga", "anthropic-api", lock(a("r_ga"))); err != nil {
			t.Fatalf("%s INSERT anthropic-api attempt: %v", as.name, err)
		}
		updates := []struct{ what, q, message string }{
			{"path of a claude-code attempt", "UPDATE attempts SET path = 'anthropic-api' WHERE id = '" + a("r_gc") + "'", "differs from mission"},
			{"path of a native attempt", "UPDATE attempts SET path = 'claude-code' WHERE id = '" + a("r_ga") + "'", "differs from mission"},
			{"mission_id of a claude-code attempt", "UPDATE attempts SET mission_id = 'm_ga' WHERE id = '" + a("r_gc") + "'", "differs from mission"},
			{"mission_id of a native attempt", "UPDATE attempts SET mission_id = 'm_gc' WHERE id = '" + a("r_ga") + "'", "differs from mission"},
			{"lock_path of a native attempt", "UPDATE attempts SET lock_path = '/elsewhere/native-locks/" + a("r_ga") + ".lock' WHERE id = '" + a("r_ga") + "'", "immutable"},
		}
		for _, u := range updates {
			_, err := as.st.pool.Exec(ctx, u.q)
			if code := sqlCode(err); (code != "23514" && code != "23001") || !strings.Contains(err.Error(), u.message) {
				t.Errorf("%s UPDATE %s: %v, want a refusal %q", as.name, u.what, err, u.message)
			}
		}
	}
	if n := count(t, owner, ctx, `SELECT count(*) FROM attempts a JOIN missions m ON m.id = a.mission_id WHERE a.path IS DISTINCT FROM m.provider`); n != 0 {
		t.Fatalf("%d attempts on a path that differs from their mission's provider", n)
	}
	if n := count(t, owner, ctx, "SELECT count(*) FROM attempts"); n != 4 {
		t.Fatalf("attempts: %d, want only the 4 positive controls", n)
	}
}

// AC-2: through the store, StartAttempt is refused unless the launcher's
// path is the mission's provider, and a native start needs its own lock
// file path; the refusal is stored, and the mission is unchanged.
func TestStartAttemptWrongPath(t *testing.T) {
	st, ctx := openTest(t)
	for _, m := range []struct {
		id domain.MissionID
		p  domain.Provider
	}{{"m_sc", domain.ProviderClaudeCode}, {"m_sa", domain.ProviderAnthropicAPI}} {
		if err := st.CreateMission(ctx, m.id, m.p, "GoodQA goodqa#15087", "", ""); err != nil {
			t.Fatal(err)
		}
		ok(t, st, ctx, "chat", "admit-"+string(m.id), domain.Command{Kind: domain.CmdAdmitMission, Mission: m.id})
	}
	lock := func(a string) string { return "/var/lib/fabric/native-locks/" + a + ".lock" }
	// A native start also names its canonical worktree (goodqa#15283).
	start := func(m domain.MissionID, a domain.AttemptID, p domain.Provider, lockPath string) domain.Command {
		c := domain.Command{Kind: domain.CmdStartAttempt, Mission: m, Attempt: a, Host: "h_t2", Role: "writer", Path: p, LockPath: lockPath}
		if p == domain.ProviderAnthropicAPI {
			c.Worktree = "/srv/wt/" + string(a)
		}
		return c
	}
	withWorktree := func(c domain.Command, wt string) domain.Command { c.Worktree = wt; return c }
	refused := []struct {
		what string
		cmd  domain.Command
	}{
		{"no path", start("m_sc", "r_s1", "", "")},
		{"unknown path", start("m_sc", "r_s2", "claude.ai", "")},
		{"anthropic-api on a claude-code mission", start("m_sc", "r_s3", domain.ProviderAnthropicAPI, lock("r_s3"))},
		{"claude-code on an anthropic-api mission", start("m_sa", "r_s4", domain.ProviderClaudeCode, "")},
		{"native start with no lock path", start("m_sa", "r_s5", domain.ProviderAnthropicAPI, "")},
		{"native start naming another attempt's lock", start("m_sa", "r_s6", domain.ProviderAnthropicAPI, lock("r_s1"))},
		{"native start with a .. segment", start("m_sa", "r_s7", domain.ProviderAnthropicAPI, "/var/lib/fabric/../native-locks/r_s7.lock")},
		{"claude-code start with a lock path", start("m_sc", "r_s8", domain.ProviderClaudeCode, lock("r_s8"))},
		// goodqa#15283 NEG-7, AC-4: the reducer half of the worktree rule.
		{"native start with no worktree", withWorktree(start("m_sa", "r_s9", domain.ProviderAnthropicAPI, lock("r_s9")), "")},
		{"native start with a relative worktree", withWorktree(start("m_sa", "r_s10", domain.ProviderAnthropicAPI, lock("r_s10")), "srv/wt")},
		{"native start with a .. worktree", withWorktree(start("m_sa", "r_s11", domain.ProviderAnthropicAPI, lock("r_s11")), "/srv/../wt")},
		{"claude-code start with a worktree", withWorktree(start("m_sc", "r_s12", domain.ProviderClaudeCode, ""), "/srv/wt")},
	}
	for i, c := range refused {
		_, err := st.Execute(ctx, "fabd", domain.CommandID(fmt.Sprintf("start-wrong-%d", i)), c.cmd)
		var ref *Refusal
		if !errors.As(err, &ref) || !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("%s: %v, want a stored invalid-transition refusal", c.what, err)
		}
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM attempts"); n != 0 {
		t.Fatalf("refused starts left %d attempts", n)
	}
	for _, id := range []domain.MissionID{"m_sc", "m_sa"} {
		if m, err := st.Mission(ctx, id); err != nil || m.Execution != domain.ExecAdmitted {
			t.Fatalf("%s after refused starts: %v %s, want ADMITTED", id, err, m.Execution)
		}
	}
	// Positive controls: each launcher on its own mission.
	ok(t, st, ctx, "fabd", "start-cc", start("m_sc", "r_sc", domain.ProviderClaudeCode, ""))
	ok(t, st, ctx, "native-runner", "start-api", start("m_sa", "r_sa", domain.ProviderAnthropicAPI, lock("r_sa")))
	var path, lp, wt string
	if err := st.pool.QueryRow(ctx, "SELECT path, lock_path, worktree FROM attempts WHERE id = 'r_sa'").Scan(&path, &lp, &wt); err != nil {
		t.Fatal(err)
	}
	if path != string(domain.ProviderAnthropicAPI) || lp != lock("r_sa") || wt != "/srv/wt/r_sa" {
		t.Fatalf("native attempt row: path %s lock_path %s worktree %s", path, lp, wt)
	}
	if n := count(t, st, ctx, "SELECT count(*) FROM attempts WHERE id = 'r_sc' AND path = 'claude-code' AND lock_path IS NULL AND worktree IS NULL"); n != 1 {
		t.Fatalf("claude-code attempt row not recorded with its path and no lock_path or worktree")
	}
}
