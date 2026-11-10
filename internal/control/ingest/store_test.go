package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/control"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "ctli")) }

// open gives a conn on an emptied, migrated test database with the control
// schema bootstrapped (the owner connection; no role grants exist yet).
func open(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	st := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := control.Bootstrap(ctx, st.Pool()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `TRUNCATE control.proj_cursor`); err != nil {
		t.Logf("truncate proj_cursor: %v", err)
	}
	c, err := pgx.Connect(ctx, storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) }) //nolint:errcheck
	return c, ctx
}

func rows(t *testing.T, ctx context.Context, c *pgx.Conn, q string) int {
	t.Helper()
	var n int
	if err := c.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ingested counts rows, bypassing the append-only trigger's irrelevance: reads only.
func ingested(t *testing.T, ctx context.Context, c *pgx.Conn) int {
	return rows(t, ctx, c, `SELECT count(*) FROM control.ingested_events`)
}

func ev(i int) string {
	return fmt.Sprintf(`{"schema":"mc.event/1","id":"harness:%d","ts":"2026-11-09T12:30:45.123Z","kind":"wal","host":"h","actor":{"agent":"a1"},"subject":{"initiative":"i1","ticket":"goodqa#1","repo":"r"},"summary":"did %d","count":2}`+"\n", i, i)
}

func run(t *testing.T, ctx context.Context, c *pgx.Conn, in string) (Result, string, error) {
	t.Helper()
	var errb bytes.Buffer
	res, err := Run(ctx, c, strings.NewReader(in), "harness", &errb)
	return res, errb.String(), err
}

// waitNotify reports whether a control_events notification arrives within d.
func waitNotify(ctx context.Context, l *pgx.Conn, d time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	n, err := l.WaitForNotification(wctx)
	return err == nil && n.Channel == NotifyChannel && n.Payload == ""
}

// AC-2: idempotent by id, with NOTIFY control_events only when a row was inserted.
func TestIngestIdempotent(t *testing.T) {
	c, ctx := open(t)
	l, err := pgx.Connect(ctx, storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background()) //nolint:errcheck
	if _, err := l.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		t.Fatal(err)
	}
	in := ev(1) + ev(2) + ev(3)
	res, _, err := run(t, ctx, c, in)
	if err != nil || res.Stored.Inserted != 3 || res.Stored.Existing != 0 || ingested(t, ctx, c) != 3 {
		t.Fatalf("first: %+v %v rows=%d", res.Stored, err, ingested(t, ctx, c))
	}
	if !waitNotify(ctx, l, 2*time.Second) {
		t.Fatal("no NOTIFY control_events after an insert")
	}
	// replay: same ids, no new rows, no notify
	res, _, err = run(t, ctx, c, in)
	if err != nil || res.Stored.Inserted != 0 || res.Stored.Existing != 3 || ingested(t, ctx, c) != 3 {
		t.Fatalf("replay: %+v %v rows=%d", res.Stored, err, ingested(t, ctx, c))
	}
	if waitNotify(ctx, l, 400*time.Millisecond) {
		t.Fatal("NOTIFY sent although nothing was inserted")
	}
	// the same id again with different content changes nothing (first write wins)
	changed := strings.Replace(ev(1), `"did 1"`, `"did something else"`, 1)
	if res, _, err = run(t, ctx, c, changed); err != nil || res.Stored.Inserted != 0 {
		t.Fatalf("changed replay: %+v %v", res.Stored, err)
	}
	var summary, actor, initiative, ticket string
	var count int
	if err := c.QueryRow(ctx, `SELECT summary, actor, initiative, ticket, count FROM control.ingested_events WHERE id = 'harness:1'`).Scan(&summary, &actor, &initiative, &ticket, &count); err != nil {
		t.Fatal(err)
	}
	if summary != "did 1" || actor != "a1" || initiative != "i1" || ticket != "goodqa#1" || count != 2 {
		t.Fatalf("stored row %q %q %q %q %d", summary, actor, initiative, ticket, count)
	}
	// a new id after a replay is the only insert and does notify
	if res, _, err = run(t, ctx, c, in+ev(4)); err != nil || res.Stored.Inserted != 1 || res.Stored.Existing != 3 {
		t.Fatalf("partial replay: %+v %v", res.Stored, err)
	}
	if !waitNotify(ctx, l, 2*time.Second) {
		t.Fatal("no NOTIFY for the new row")
	}
}

// AC-2: the ingest writes control.ingested_events and nothing else.
func TestNeverWritesFabricEvents(t *testing.T) {
	c, ctx := open(t)
	snap := func() string {
		var s string
		if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM events)||'/'||(SELECT count(*) FROM outbox)||'/'||(SELECT count(*) FROM commands)||'/'||
			(SELECT count(*) FROM missions)||'/'||(SELECT count(*) FROM outbox_cursors)||'/'||(SELECT count(*) FROM control.mc_chat_mirror)||'/'||
			(SELECT count(*) FROM control.pulses)||'/'||(SELECT count(*) FROM control.initiative_state)||'/'||(SELECT count(*) FROM control.proj_cursor)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	// a hostile mix: valid, fabric: ids, wrong source, junk
	in := ev(1) + ev(2) + strings.Replace(ev(3), "harness:3", "fabric:3", 1) + `{"schema":"mc.event/1","id":"fabric:9","ts":"2026-11-09T12:30:45Z","kind":"wal","source":"fabric"}` + "\nnot json\n"
	res, _, err := run(t, ctx, c, in)
	if err != nil || res.Stored.Inserted != 2 {
		t.Fatalf("%+v %v", res.Stored, err)
	}
	if after := snap(); after != before {
		t.Fatalf("ingest wrote outside control.ingested_events: %s -> %s", before, after)
	}
	if n := rows(t, ctx, c, `SELECT count(*) FROM control.ingested_events WHERE id LIKE 'fabric:%' OR source = 'fabric'`); n != 0 {
		t.Fatalf("%d fabric rows stored", n)
	}
}

// NEG-2: an external line claiming a fabric: id is rejected (and counted), the rest still land.
func TestExternalFabricIDRejected(t *testing.T) {
	c, ctx := open(t)
	in := strings.Replace(ev(1), "harness:1", "fabric:1", 1) + ev(2)
	res, errs, err := run(t, ctx, c, in)
	if err != nil || len(res.Batch.Rejected) != 1 || res.Batch.Rejected[0].Reason != "reserved-id-prefix" || res.Stored.Inserted != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(errs, "rejected=1") || !strings.Contains(errs, "reserved-id-prefix") || strings.Contains(errs, "fabric:1") {
		t.Fatalf("stderr report: %q", errs)
	}
	// the reserved source is refused outright
	var b bytes.Buffer
	if _, err := Run(ctx, c, strings.NewReader(ev(1)), "fabric", &b); err == nil {
		t.Fatal("source fabric accepted")
	}
}

// Defence in depth: even a caller that skips the validator cannot store a fabric
// row, and a database error mid-batch leaves nothing behind (one transaction).
func TestStoreAtomicAndDBBackstop(t *testing.T) {
	c, ctx := open(t)
	good := Event{ID: "harness:g", TS: time.Now().UTC().Truncate(time.Millisecond), Kind: "wal", Source: "harness", Summary: "ok", Severity: "info", Count: 1}
	bad := Event{ID: "fabric:1", TS: good.TS, Kind: "wal", Source: "fabric", Summary: "forged", Severity: "info", Count: 1}
	if _, err := Store(ctx, c, []Event{good, bad}); err == nil {
		t.Fatal("a forged fabric row was stored")
	}
	if n := ingested(t, ctx, c); n != 0 {
		t.Fatalf("%d rows survived a failed batch", n)
	}
	// source other than the id prefix is refused by the table
	bad2 := good
	bad2.ID, bad2.Source = "harness:h", "other"
	if _, err := Store(ctx, c, []Event{bad2}); err == nil {
		t.Fatal("source differing from the id prefix was stored")
	}
	// an over-long field is cut to the column limit, not an error that aborts a batch
	long := good
	long.ID = "harness:long"
	long.Actor = map[string]string{"agent": strings.Repeat("a", 160)}
	long.Subject = map[string]string{"initiative": strings.Repeat("i", 160), "ticket": strings.Repeat("t", 160)}
	long.Count = 1_000_000_000
	if st, err := Store(ctx, c, []Event{long}); err != nil || st.Inserted != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if n := rows(t, ctx, c, `SELECT count(*) FROM control.ingested_events WHERE char_length(actor) = 128 AND char_length(initiative) = 128 AND char_length(ticket) = 64 AND count = 1000000`); n != 1 {
		t.Fatalf("limits not applied: %d", n)
	}
}

// AC-3 through the store: 2000 lines land; 2001 lines or over 1 MiB write nothing.
func TestIngestLimitsEndToEnd(t *testing.T) {
	c, ctx := open(t)
	var all strings.Builder
	for i := 0; i < MaxLines; i++ {
		all.WriteString(ev(i))
	}
	res, errs, err := run(t, ctx, c, all.String())
	if err != nil || res.Stored.Inserted != MaxLines || ingested(t, ctx, c) != MaxLines || !strings.Contains(errs, "lines=2000 accepted=2000 inserted=2000") {
		t.Fatalf("%+v %v rows=%d\n%s", res.Stored, err, ingested(t, ctx, c), errs)
	}
	before := ingested(t, ctx, c)
	if _, _, err := run(t, ctx, c, all.String()+ev(99999)); !errors.Is(err, ErrCallTooLarge) {
		t.Fatalf("2001 lines: %v", err)
	}
	if _, _, err := run(t, ctx, c, strings.Repeat("x", MaxCallBytes+1)); !errors.Is(err, ErrCallTooLarge) {
		t.Fatalf("1 MiB+1: %v", err)
	}
	if n := ingested(t, ctx, c); n != before {
		t.Fatalf("a refused call wrote rows: %d -> %d", before, n)
	}
}

// NEG-1: oversized, truncated, wrong-version and non-UTF-8 input: no crash, no
// partial write of a bad line; valid neighbours still land; counts on stderr.
func TestHostileInputNoPartialWrite(t *testing.T) {
	c, ctx := open(t)
	big := `{"schema":"mc.event/1","id":"harness:big","ts":"2026-11-09T12:30:45Z","kind":"wal","host":"` + strings.Repeat("a", MaxLineBytes) + `"}` + "\n"
	in := ev(1) + big + ev(2)[:60] + "\n" + `{"schema":"mc.event/7","id":"harness:v7","ts":"2026-11-09T12:30:45Z","kind":"wal"}` + "\n" +
		"\xff\xfe\x00{}\n" + `{"schema":"mc.event/1","id":"harness:nonutf8","ts":"2026-11-09T12:30:45Z","kind":"wal","summary":"a` + "\xff" + `b"}` + "\n" + ev(3)
	res, errs, err := run(t, ctx, c, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored.Inserted != 2 || len(res.Batch.Rejected) != 5 || ingested(t, ctx, c) != 2 {
		t.Fatalf("inserted=%d rejected=%+v rows=%d", res.Stored.Inserted, res.Batch.Rejected, ingested(t, ctx, c))
	}
	for _, id := range []string{"harness:big", "harness:v7", "harness:nonutf8"} {
		if n := rows(t, ctx, c, fmt.Sprintf(`SELECT count(*) FROM control.ingested_events WHERE id = '%s'`, id)); n != 0 {
			t.Errorf("%s was stored", id)
		}
	}
	if !strings.Contains(errs, "rejected=5") || !strings.Contains(errs, "line-too-long") || !strings.Contains(errs, "unknown-version") || !strings.Contains(errs, "not-utf8") {
		t.Fatalf("stderr counts:\n%s", errs)
	}
	// all-invalid input writes nothing and still succeeds with counts
	res, _, err = run(t, ctx, c, "junk\n"+"\xff\n")
	if err != nil || res.Stored.Inserted != 0 || ingested(t, ctx, c) != 2 {
		t.Fatalf("%+v %v", res.Stored, err)
	}
}

// Review (privacy): a secret in any text field never reaches the append-only table.
func TestSecretsNeverStored(t *testing.T) {
	c, ctx := open(t)
	f := fakes()
	in := `{"schema":"mc.event/1","id":"harness:s1","ts":"2026-11-09T12:30:45Z","kind":"wal","host":"` + f["bearer"] + `","actor":{"agent":"` + f["slack"] +
		`"},"subject":{"ref":"` + f["aws-key"] + `"},"summary":"token ` + f["github"] + ` leaked"}` + "\n"
	res, errs, err := run(t, ctx, c, in)
	if err != nil || res.Stored.Inserted != 1 || !strings.Contains(errs, "redacted-fields=4") {
		t.Fatalf("%+v %v\n%s", res.Stored, err, errs)
	}
	var all string
	if err := c.QueryRow(ctx, `SELECT summary || '|' || host || '|' || actor || '|' || ref FROM control.ingested_events WHERE id = 'harness:s1'`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"github", "bearer", "slack", "aws-key"} {
		if strings.Contains(all, f[k]) || strings.Contains(errs, f[k]) {
			t.Errorf("%s stored or printed: %q", k, all)
		}
	}
	if strings.Count(all, "[REDACTED:") != 4 {
		t.Fatalf("stored %q", all)
	}
}

// Review (availability): concurrent ingests of the same ids in opposite orders
// must not deadlock; both complete and the rows land once.
func TestConcurrentIngestNoDeadlock(t *testing.T) {
	c, ctx := open(t)
	const n = 200
	for round := 0; round < 5; round++ {
		var fwd, rev strings.Builder
		for i := 0; i < n; i++ {
			fwd.WriteString(ev(round*1000 + i))
		}
		for i := n - 1; i >= 0; i-- {
			rev.WriteString(ev(round*1000 + i))
		}
		c2, err := pgx.Connect(ctx, storetest.DSN)
		if err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 2)
		go func() { _, _, e := run(t, ctx, c, fwd.String()); errs <- e }()
		go func() {
			var b bytes.Buffer
			_, e := Run(ctx, c2, strings.NewReader(rev.String()), "harness", &b)
			errs <- e
		}()
		for i := 0; i < 2; i++ {
			if e := <-errs; e != nil {
				t.Fatalf("round %d: %v", round, e)
			}
		}
		c2.Close(context.Background()) //nolint:errcheck
	}
	if got := ingested(t, ctx, c); got != 5*n {
		t.Fatalf("%d rows, want %d", got, 5*n)
	}
}

// NEG-3: control characters and ANSI in any text field never reach the table.
func TestControlCharactersNeverStored(t *testing.T) {
	c, ctx := open(t)
	in := strings.Replace(ev(1), `"did 1"`, `"\u001b[31mred\u001b[0m"`, 1) +
		strings.Replace(ev(2), `"did 2"`, `"line1\nline2"`, 1) +
		strings.Replace(ev(3), `"host":"h"`, `"host":"a\u0007b"`, 1) + ev(4)
	res, _, err := run(t, ctx, c, in)
	if err != nil || res.Stored.Inserted != 1 || len(res.Batch.Rejected) != 3 {
		t.Fatalf("%+v %v %+v", res.Stored, err, res.Batch.Rejected)
	}
	if n := rows(t, ctx, c, `SELECT count(*) FROM control.ingested_events WHERE summary ~ '[[:cntrl:]]' OR host ~ '[[:cntrl:]]'`); n != 0 {
		t.Fatalf("%d rows with control characters", n)
	}
}

func TestMarkerCredentialDoesNotReachStoredRows(t *testing.T) {
	c, ctx := open(t)
	canary := strings.Repeat("Q", 18)
	value := "password=[REDACTED:named-credential]" + canary
	line := `{"schema":"mc.event/1","id":"harness:marker-stored","ts":"2026-11-09T12:30:45Z","kind":"wal","host":"` + value + `","tenant":"` + value + `","summary":"` + value + `","actor":{"agent":"` + value + `"},"subject":{"repo":"` + value + `","branch":"` + value + `","ref":"` + value + `","ticket":"` + value + `","initiative":"` + value + `"}}`
	res, stderr, err := run(t, ctx, c, line)
	if err != nil || res.Stored.Inserted != 1 || strings.Contains(stderr, canary) {
		t.Fatal("marker redaction did not safely store/report event")
	}
	var leaked int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM control.ingested_events WHERE strpos(concat(summary,host,tenant,actor,repo,branch,ref,ticket,initiative),$1)>0`, canary).Scan(&leaked); err != nil || leaked != 0 {
		t.Error("marker-bearing credential reached stored rows")
	}
}
