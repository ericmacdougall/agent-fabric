package metrics

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "met")) }

// open returns an emptied store's pool; sched_counters is not covered by
// Truncate.
func open(t *testing.T) *pgxpool.Pool {
	t.Helper()
	st := storetest.Open(t)
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM sched_counters`); err != nil {
		t.Fatal(err)
	}
	return st.Pool()
}

// exec runs statements in one transaction with triggers off, so rows can be
// seeded directly in any state.
func exec(t *testing.T, pool *pgxpool.Pool, qs ...string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range append([]string{"SET LOCAL session_replication_role = replica"}, qs...) {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// hostileTitle stands in for ticket text and must never reach the output.
const hostileTitle = `goodqa#999 SECRET-TITLE sk-live-xyz "} injected{a="b`

func seed(t *testing.T, pool *pgxpool.Pool) {
	exec(t, pool,
		`INSERT INTO missions (id, ticket, execution, terminal, state, provider) VALUES
			('m_1', 'goodqa#1', 'RUNNING', 'NONE', '{}', 'claude-code'),
			('m_2', 'goodqa#2', 'IDLE', 'NONE', '{}', 'claude-code'),
			('m_3', '`+strings.ReplaceAll(hostileTitle, "'", "''")+`', 'RUNNING"} x{y="1', 'NONE', '{}', 'claude-code')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, lease_until, path) VALUES
			('a_1', 'm_1', 1, 0, 0, 'RUNNING', now() + interval '1 hour', 'claude-code'),
			('a_2', 'm_2', 1, 0, 0, 'LEASED', now() - interval '1 hour', 'claude-code'),
			('a_3', 'm_1', 2, 0, 0, 'ENDED', NULL, 'claude-code')`,
		`INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, dispatches, updated_at) VALUES
			('e_1', 'm_1', 'a_1', 1, 'ticket_comment', 's1', 'k1', '{}', 'x', 'CONFIRMED', 1, now()),
			('e_2', 'm_1', 'a_1', 1, 'publish', 's2', 'k2', '{}', 'x', 'QUARANTINED', 0, now() - interval '100 seconds'),
			('e_3', 'm_1', 'a_1', 1, 'ticket_comment', 's3', 'k3', '{}', 'x', 'DISPATCHED', 1, now()),
			('e_4', 'm_1', 'a_1', 1, 'publish', 's4', 'k4', '{}', 'x', 'DISPATCHED', 1, now()),
			('e_5', 'm_1', 'a_1', 1, 'ticket_comment', 's5', 'k5', '{}', 'x', 'UNKNOWN', 1, now() - interval '30 seconds')`,
		`INSERT INTO effect_dispatches (effect_id, dispatch, deadline) VALUES
			('e_3', 1, now() - interval '10 seconds'), ('e_4', 1, now() + interval '60 seconds')`,
		`INSERT INTO asks (id, kind, question, source, state, created_at) VALUES
			('k_1', 'decision', '`+strings.ReplaceAll(hostileTitle, "'", "''")+`', 'user', 'open', now() - interval '50 seconds'),
			('k_2', 'permission', 'q', 'user', 'open', now() - interval '10 seconds')`,
		`INSERT INTO asks (id, kind, question, source, state, answer, answered_by, answered_at, answer_source, created_at) VALUES
			('k_3', 'decision', 'q', 'user', 'answered', 'yes', 'user', now(), 'user', now() - interval '900 seconds')`,
		`INSERT INTO sched_counters (name, value) VALUES ('runs_launched_total', 3)`,
		`INSERT INTO resource_capacities (resource, unit, capacity, held) VALUES ('usd_api', 'usd_micros', 100000000, 2000000)`,
		`INSERT INTO hold_commands (key, request_sha256) VALUES ('h1', 'x')`,
		`INSERT INTO reservations (id, hold_key, attempt_id, fence, resource, unit, amount, generation, state, expires_at, closed_at) VALUES
			('r_1', 'h1', 'a_1', 1, 'usd_api', 'usd_micros', 3000000, 1, 'SETTLED', now() + interval '1 hour', now())`,
		`INSERT INTO usage_receipts (resource, receipt_id, reservation_id, actual) VALUES ('usd_api', 'rc1', 'r_1', 1500000)`,
		`INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha, cost_micros) VALUES
			('a_1', 1, 'assistant', '\x00', 1, 'x', 250000)`,
	)
}

var opts = Options{Version: "v1.2.3-test", DispatchLimit: 8,
	Caps: []Cap{{"month", 100_000_000}, {"job", 5_000_000}, {"live_test", 1_000_000}}}

func render(t *testing.T, pool *pgxpool.Pool, o Options) string {
	t.Helper()
	s, err := Collect(context.Background(), pool, o)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var b bytes.Buffer
	if err := s.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// AC: the exposition for a seeded store matches testdata/golden.prom
// byte for byte (names, help, types, labels, order, values).
func TestExpositionGolden(t *testing.T) {
	pool := open(t)
	seed(t, pool)
	got := render(t, pool, opts)
	want, err := os.ReadFile("testdata/golden.prom")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("exposition differs from testdata/golden.prom:\n--- got\n%s", got)
	}
}

// sample returns one sample's value from an exposition, or -1.
func sample(out, line string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, line+" ") {
			return strings.TrimPrefix(l, line+" ")
		}
	}
	return "absent"
}

// AC: counters move after a seeded mission, effect and overdue dispatch.
func TestCountersMove(t *testing.T) {
	pool := open(t)
	before := render(t, pool, opts)
	seed(t, pool)
	after := render(t, pool, opts)
	for _, c := range []struct{ line, before, after string }{
		{`fabric_missions{execution="RUNNING",terminal="NONE"}`, "absent", "1"},
		{`fabric_effects{kind="publish",state="QUARANTINED"}`, "absent", "1"},
		{`fabric_dispatch_overdue`, "0", "1"},
		{`fabric_dispatch_inflight`, "0", "2"},
		{`fabric_asks_open_oldest_age_seconds`, "0", "50"},
		{`fabric_effect_oldest_age_seconds{state="QUARANTINED"}`, "0", "100"},
		{`fabric_sched_counter_total{name="runs_launched_total"}`, "absent", "3"},
	} {
		if b, a := sample(before, c.line), sample(after, c.line); b != c.before || a != c.after {
			t.Errorf("%s: before %s after %s, want %s then %s", c.line, b, a, c.before, c.after)
		}
	}
}

// lineRE is every non-comment line the writer may emit: label values are
// only ids and enums.
var lineRE = regexp.MustCompile(`^fabric_[a-z_]+(\{[a-z_]+="[A-Za-z0-9_.:-]{1,64}"(,[a-z_]+="[A-Za-z0-9_.:-]{1,64}")*\})? [0-9]+$`)

// NEG (sanitizer): hostile free text in enum and name columns never reaches
// a label; ticket and question text is never read at all.
func TestHostileLabels(t *testing.T) {
	pool := open(t)
	seed(t, pool)
	exec(t, pool,
		`INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state) VALUES
			('e_9', 'm_1', 'a_1', 1, E'kind\nfabric_up 0', '/home/eric/secret path', 'k9', '{}', 'x', 'CONFIRMED')`,
		`INSERT INTO sched_counters (name, value) VALUES ('postgres://user:pw@db/x', 7)`)
	out := render(t, pool, opts)
	for _, bad := range []string{"SECRET", "goodqa#", "sk-live", "injected", "x{y", "/home/eric", "postgres://", "pw@", "fabric_up 0"} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q", bad)
		}
	}
	for _, want := range []string{`fabric_missions{execution="other",terminal="NONE"} 1`,
		`fabric_effects{kind="other",state="CONFIRMED"} 1`, `fabric_sched_counter_total{name="other"} 7`} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output lacks %s", want)
		}
	}
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "# ") && !lineRE.MatchString(l) {
			t.Errorf("unexpected exposition line %q", l)
		}
	}
}

func TestLabel(t *testing.T) {
	for v, want := range map[string]string{
		"RUNNING": "RUNNING", "ticket_comment": "ticket_comment", "usd_api": "usd_api", "a.b:c-d": "a.b:c-d",
		"": "other", "a b": "other", `a"b`: "other", "a\nb": "other", "é": "other", "a/b": "other",
		strings.Repeat("x", 64): strings.Repeat("x", 64), strings.Repeat("x", 65): "other",
	} {
		if got := Label(v); got != want {
			t.Errorf("Label(%q) = %q, want %q", v, got, want)
		}
	}
}

// A collector facing a held row lock returns at once with correct counts:
// it reads a snapshot and takes no row locks (a FOR UPDATE in it is refused
// by the read-only transaction, mutant M313). Under MVCC a plain reader never
// blocks a dispatch's UPDATE; the remaining interaction, a migration's
// ACCESS EXCLUSIVE request queued behind the collector, is bounded by the
// collector's deadline (review N3 on 4e3f8b1; see Collect).
func TestCollectIgnoresRowLocks(t *testing.T) {
	pool := open(t)
	seed(t, pool)
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM effects WHERE id = 'e_3' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out := render(t, pool, Options{Timeout: 500 * time.Millisecond})
	if d := time.Since(start); d > 400*time.Millisecond || sample(out, "fabric_dispatch_inflight") != "2" {
		t.Fatalf("collect under a row lock took %s: %s", d, sample(out, "fabric_dispatch_inflight"))
	}
}

// NEG (timeout): under a table lock that blocks every reader, the
// collector gives up at its own timeout with an error, not at the caller's.
func TestCollectLockTimeout(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `LOCK TABLE effects IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	// Review N2 on 4e3f8b1: a sub-millisecond timeout must not become
	// statement_timeout = 0 (no limit); it is clamped to 1 ms.
	for _, timeout := range []time.Duration{200 * time.Millisecond, 500 * time.Microsecond, time.Nanosecond} {
		start := time.Now()
		_, err = Collect(ctx, pool, Options{Timeout: timeout})
		if d := time.Since(start); err == nil || d > 900*time.Millisecond {
			t.Fatalf("collect (timeout %s) under an exclusive lock: %v after %s", timeout, err, d)
		}
	}
}

// NEG (loopback): a non-loopback listen address is refused without
// --allow-remote.
func TestCheckListen(t *testing.T) {
	for _, a := range []string{"127.0.0.1:9464", "[::1]:9464", "127.0.0.2:0"} {
		if err := CheckListen(a, false); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:9464", ":9464", "[::]:9464", "10.0.0.5:9464", "192.168.1.2:9464",
		"localhost:9464", "example.com:9464", "127.0.0.1.nip.io:9464"} {
		if err := CheckListen(a, false); err == nil {
			t.Errorf("%s accepted without --allow-remote", a)
		}
		if err := CheckListen(a, true); err != nil {
			t.Errorf("%s refused with --allow-remote: %v", a, err)
		}
	}
	for _, a := range []string{"127.0.0.1", "127.0.0.1:http", "127.0.0.1:70000", "127.0.0.1:-1", "127.0.0.1:09464"} {
		if err := CheckListen(a, true); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
}

func TestHandler(t *testing.T) {
	const listen = "127.0.0.1:9464"
	ok := func(context.Context) (*Snapshot, error) {
		return &Snapshot{Families: []Family{gauge("fabric_dispatch_inflight", "h", 2)}}, nil
	}
	fail := func(context.Context) (*Snapshot, error) {
		return nil, errors.New("dial postgres://user:pw@db: refused")
	}
	for _, c := range []struct {
		method, path, host, origin string
		collect                    func(context.Context) (*Snapshot, error)
		code                       int
		body                       string
	}{
		{"GET", "/metrics", listen, "", ok, 200, "fabric_dispatch_inflight 2\n"},
		{"GET", "/metrics", "127.0.0.1", "", ok, 200, "fabric_dispatch_inflight 2\n"},
		{"GET", "/metrics", "[::1]:9464", "", ok, 200, ""},
		{"HEAD", "/metrics", listen, "", ok, 200, ""},
		{"POST", "/metrics", listen, "", ok, 405, ""},
		{"PUT", "/metrics", listen, "", ok, 405, ""},
		{"DELETE", "/metrics", listen, "", ok, 405, ""},
		{"GET", "/", listen, "", ok, 404, ""},
		{"GET", "/metrics/x", listen, "", ok, 404, ""},
		{"GET", "/metrics", listen, "", fail, 503, ""},
		// Review N1 on 4e3f8b1 (DNS rebinding): a host name, or any request a
		// browser marks with Origin, is refused.
		{"GET", "/metrics", "evil.example:9464", "", ok, 403, ""},
		{"GET", "/metrics", "localhost:9464", "", ok, 403, ""},
		{"GET", "/metrics", "", "", ok, 403, ""},
		{"GET", "/metrics", listen, "http://evil.example", ok, 403, ""},
		{"GET", "/metrics", listen, "null", ok, 403, ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		Handler(listen, c.collect).ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s %s host %q origin %q: %d, want %d", c.method, c.path, c.host, c.origin, rec.Code, c.code)
		}
		if c.body != "" && !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s %s: body %q", c.method, c.path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "pw@") || strings.Contains(rec.Body.String(), "postgres") {
			t.Errorf("%s %s: error detail leaked: %q", c.method, c.path, rec.Body.String())
		}
		if c.code == 200 && rec.Header().Get("Content-Type") != ContentType {
			t.Errorf("content type %q", rec.Header().Get("Content-Type"))
		}
	}
	// One collection at a time.
	in, out := make(chan struct{}), make(chan struct{})
	slow := func(context.Context) (*Snapshot, error) { close(in); <-out; return &Snapshot{}, nil }
	h := Handler(listen, slow)
	get := func() *http.Request { r := httptest.NewRequest("GET", "/metrics", nil); r.Host = listen; return r }
	go h.ServeHTTP(httptest.NewRecorder(), get())
	<-in
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, get())
	close(out)
	if rec.Code != 503 {
		t.Errorf("concurrent scrape: %d, want 503", rec.Code)
	}
}

// goodqa#15623 AC-14: the Control Room tables' row estimates are exported when the schema
// exists, from the catalog alone, and the family is empty before the bootstrap.
func TestControlRowsEstimate(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	out := render(t, pool, opts)
	if !strings.Contains(out, "# TYPE fabric_control_rows_estimate gauge") || strings.Contains(out, "fabric_control_rows_estimate{") {
		t.Fatalf("before the bootstrap the family must exist and have no samples:\n%s", out)
	}
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS control CASCADE`,
		`CREATE SCHEMA control`,
		`CREATE TABLE control.ingested_events (a int)`,
		`INSERT INTO control.ingested_events SELECT generate_series(1, 50)`,
		`ANALYZE control.ingested_events`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS control CASCADE`) }) //nolint:errcheck
	out = render(t, pool, opts)
	if got := sample(out, `fabric_control_rows_estimate{table="ingested_events"}`); got != "50" {
		t.Fatalf("estimate for control.ingested_events = %s, want 50:\n%s", got, out)
	}
}
