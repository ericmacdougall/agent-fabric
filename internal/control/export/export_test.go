package export

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
	"fabric/internal/store"
	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "ctlx")) }

const secret = "PAYLOADSECRET-7f3a"

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func conn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) }) //nolint:errcheck
	return c
}

// run exports as the given connection and returns the event lines, the final
// cursor and the result.
func run(t *testing.T, ctx context.Context, db DB, since string) ([]map[string]any, string, Result) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Export(ctx, db, since, &buf)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	evs, cur := parseOutput(t, buf.Bytes())
	if cur != res.Cursor {
		t.Fatalf("cursor line %q, result %q", cur, res.Cursor)
	}
	return evs, cur, res
}

func parseOutput(t *testing.T, out []byte) ([]map[string]any, string) {
	t.Helper()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("output does not end in a newline (cut mid-line?)")
	}
	var evs []map[string]any
	var cursor string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if cursor != "" {
			t.Fatalf("a line after the cursor line: %q", sc.Text())
		}
		var o map[string]any
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("line is not JSON (cut?): %v: %.80q", err, sc.Text())
		}
		if c, ok := o["cursor"].(string); ok {
			if len(o) != 1 {
				t.Fatalf("cursor line has other fields: %v", o)
			}
			cursor = c
			continue
		}
		evs = append(evs, o)
	}
	if cursor == "" {
		t.Fatal("output has no cursor line")
	}
	return evs, cursor
}

// seed creates n admitted missions.
func seed(t *testing.T, st *store.Store, ctx context.Context, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := domain.MissionID(fmt.Sprintf("m_%s%d", prefix, i))
		if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#1 "+secret, "fabric", "main"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Execute(ctx, "chat", domain.CommandID("admit:"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
}

func fresh(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st := storetest.Open(t)
	ctx := ctxT(t)
	if err := st.SetCap(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

// rawEvent inserts an event and its outbox row with an arbitrary kind and payload.
func rawEvent(t *testing.T, ctx context.Context, tx pgx.Tx, mission string, seq int, kind, payload string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload) VALUES ($1, $2, $3, 'test', $4, $5::jsonb)`,
		mission, seq, kind, fmt.Sprintf("raw-%s-%d", mission, seq), payload); err != nil {
		t.Fatalf("raw event: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, mission, seq, kind, payload); err != nil {
		t.Fatalf("raw outbox: %v", err)
	}
}

func rawEvents(t *testing.T, st *store.Store, ctx context.Context, mission string, rows ...[2]string) {
	t.Helper()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var base int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM events WHERE mission_id = $1`, mission).Scan(&base); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		rawEvent(t, ctx, tx, mission, base+i+1, r[0], r[1])
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func lagEvents(evs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if strings.HasPrefix(e["id"].(string), "fabric:lag:") {
			out = append(out, e)
		}
	}
	return out
}

func withoutLag(evs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if !strings.HasPrefix(e["id"].(string), "fabric:lag:") {
			out = append(out, e)
		}
	}
	return out
}

func idsOf(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["id"].(string))
	}
	return out
}

// AC-2: a second call from the returned cursor with no new events gives no
// event lines and the same cursor; one new event gives exactly that event.
func TestCursorResume(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "r", 4)
	first, cur, _ := run(t, ctx, c, "")
	if len(first) == 0 {
		t.Fatal("no events exported")
	}
	again, cur2, res := run(t, ctx, c, cur)
	if len(again) != 0 || res.Events != 0 || cur2 != cur {
		t.Fatalf("second call: %d events, cursor %q -> %q", len(again), cur, cur2)
	}
	seed(t, st, ctx, "s", 1)
	next, cur3, _ := run(t, ctx, c, cur)
	if len(next) == 0 || cur3 == cur {
		t.Fatalf("new event not exported: %d events", len(next))
	}
	for _, e := range next {
		for _, f := range first {
			if e["id"] == f["id"] {
				t.Fatalf("event %v repeated after the cursor", e["id"])
			}
		}
	}
}

// An empty database gives only a cursor line, and that cursor resumes.
func TestEmptyStore(t *testing.T) {
	_, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	evs, cur, _ := run(t, ctx, c, "")
	if len(evs) != 0 {
		t.Fatalf("events from an empty store: %v", evs)
	}
	if evs, cur2, _ := run(t, ctx, c, cur); len(evs) != 0 || cur2 != cur {
		t.Fatalf("resume from empty: %v %q %q", evs, cur, cur2)
	}
}

// AC-2: the 1 MiB cap, never cut mid-line; paging by cursor loses and repeats nothing.
func TestOutputCap(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "cap", 1)
	const n = 7000
	if _, err := st.Pool().Exec(ctx, `INSERT INTO events (mission_id, seq, kind, actor, command_id, payload)
			SELECT 'm_cap0', 100 + g, 'EVIDENCE_RECORDED', 'test', 'bulk' || g, '{}'::jsonb FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO outbox (mission_id, seq, kind, payload)
			SELECT 'm_cap0', 100 + g, 'EVIDENCE_RECORDED', '{}'::jsonb FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cur, pages := "", 0
	for {
		var buf bytes.Buffer
		res, err := Export(ctx, c, cur, &buf)
		if err != nil {
			t.Fatal(err)
		}
		if buf.Len() > MaxOutput {
			t.Fatalf("output %d bytes exceeds the %d cap", buf.Len(), MaxOutput)
		}
		evs, cursor := parseOutput(t, buf.Bytes()) // fails on a cut line
		for _, e := range evs {
			if e["historic"] != true {
				t.Fatalf("backfill chain event %v lacks historic=true (page %d)", e["id"], pages+1)
			}
		}
		for _, id := range idsOf(evs) {
			if seen[id] {
				t.Fatalf("%s repeated across pages", id)
			}
			seen[id] = true
		}
		pages++
		cur = cursor
		if !res.More {
			break
		}
		if pages == 1 && buf.Len() < MaxOutput-1024 {
			t.Fatalf("cap stopped the page early at %d bytes", buf.Len())
		}
	}
	if pages < 2 {
		t.Fatalf("expected the cap to split %d events into pages, got %d page", total, pages)
	}
	if len(seen) != total {
		t.Fatalf("exported %d of %d events", len(seen), total)
	}
}

// AC-3: one read-only transaction, and the app role can run it.
type spyDB struct {
	db   DB
	opts pgx.TxOptions
	n    int
	t    *testing.T
	ctx  context.Context
}

func (s *spyDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	s.n++
	s.opts = o
	tx, err := s.db.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	var ro string
	if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro); err != nil || ro != "on" {
		s.t.Errorf("transaction_read_only = %q (%v)", ro, err)
	}
	// A write inside this very transaction must be refused by the server.
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursors (consumer, last_id) VALUES ('export-must-not-write', 1)`); err == nil {
		s.t.Error("a write succeeded inside the export transaction")
	}
	// The failed statement aborted the tx; the exporter must still be handed a
	// usable one, so give it a fresh read-only transaction.
	tx.Rollback(ctx) //nolint:errcheck
	return s.db.BeginTx(ctx, o)
}

func TestExportReadOnly(t *testing.T) {
	st, ctx := fresh(t)
	seed(t, st, ctx, "ro", 3)
	appDSN, _ := storetest.AppRunnerDSNs(t, st)
	app := conn(t, ctx, appDSN)
	snap := func() string {
		var s string
		if err := st.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM events)||'/'||(SELECT count(*) FROM outbox)||'/'||(SELECT count(*) FROM outbox_cursors)||'/'||(SELECT count(*) FROM missions)||'/'||(SELECT count(*) FROM commands)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	spy := &spyDB{db: app, t: t, ctx: ctx}
	evs, _, _ := run(t, ctx, spy, "")
	if len(evs) == 0 {
		t.Fatal("the app role exported nothing")
	}
	if spy.n != 1 {
		t.Fatalf("%d transactions, want 1", spy.n)
	}
	if spy.opts.AccessMode != pgx.ReadOnly || spy.opts.IsoLevel != pgx.RepeatableRead {
		t.Fatalf("tx options %+v", spy.opts)
	}
	if after := snap(); after != before {
		t.Fatalf("export changed the store: %s -> %s", before, after)
	}
}

// AC-4: stale, foreign and malformed cursors are refused with ErrBadCursor and
// nothing is written to the output.
func TestStaleCursor(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "st", 5)
	_, cur, _ := run(t, ctx, c, "")
	_, id, rh, _, err := decodeCursor(cur)
	if err != nil || id < 2 {
		t.Fatalf("cursor %q: id %d %v", cur, id, err)
	}
	dbid, _, _, _, _ := decodeCursor(cur)
	foreign := encodeCursor("0123456789abcdef", id, rh, false)
	if dbid == "0123456789abcdef" {
		t.Fatal("collision")
	}
	tampered := cur[:len(cur)-1] + map[bool]string{true: "0", false: "1"}[cur[len(cur)-1] == '1']
	cases := map[string]string{
		"garbage":   "not-a-cursor",
		"empty-ish": "fx1..",
		"tampered":  tampered,
		"foreign":   foreign,
		"numeric":   "42",
		"zero":      "0",
	}
	for name, since := range cases {
		var buf bytes.Buffer
		_, err := Export(ctx, c, since, &buf)
		if !errors.Is(err, ErrBadCursor) {
			t.Errorf("%s: want ErrBadCursor, got %v", name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: wrote %d bytes before refusing", name, buf.Len())
		}
	}
	// Stale: the store is rebuilt with fewer rows, so the old cursor names a row that is gone.
	if err := st.Truncate(ctx); err != nil {
		t.Fatal(err)
	}
	seed(t, st, ctx, "n", 1)
	var buf bytes.Buffer
	_, err = Export(ctx, c, cur, &buf)
	if !errors.Is(err, ErrBadCursor) || !strings.Contains(err.Error(), "stale") || buf.Len() != 0 {
		t.Fatalf("stale cursor: err %v, %d bytes written", err, buf.Len())
	}
}

// NEG-1: a held stream and an unknown kind export as kind system with a fixed
// summary; the payload marker never appears anywhere in the output.
func TestHeldAndUnknownNeverLeak(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "h", 3)
	payload := `{"note":"` + secret + `","ticket":"` + secret + `"}`
	rawEvents(t, st, ctx, "m_h0", [2]string{"FROM_THE_FUTURE", payload}, [2]string{"MISSION_ADMITTED", payload})
	if err := st.HoldMission(ctx, st.Pool(), "m_h1"); err != nil {
		t.Fatal(err)
	}
	rawEvents(t, st, ctx, "m_h1", [2]string{"MISSION_ADMITTED", payload})
	var buf bytes.Buffer
	if _, err := Export(ctx, c, "", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("payload text leaked:\n%s", buf.String())
	}
	evs, _ := parseOutput(t, buf.Bytes())
	var unknown, held, normal int
	for _, e := range evs {
		sum := e["summary"].(string)
		switch {
		case strings.Contains(sum, "unrecognized kind"):
			unknown++
			if e["kind"] != "system" {
				t.Errorf("unknown kind exported as %v", e["kind"])
			}
		case strings.Contains(sum, "stream held"):
			held++
			if e["kind"] != "system" || e["subject"].(map[string]any)["initiative"] != "m_h1" {
				t.Errorf("held event: %v", e)
			}
		default:
			normal++
		}
	}
	// m_h1 has no scheduler cursor yet, so all of its rows (2) are held.
	if unknown != 1 || held != 2 || normal != 3 {
		t.Fatalf("unknown=%d held=%d normal=%d; nothing may be dropped:\n%s", unknown, held, normal, buf.String())
	}
	var total int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if len(evs) != total {
		t.Fatalf("exported %d of %d outbox rows", len(evs), total)
	}
}

// NEG-2, deterministic: a row that commits later with a lower id is not passed
// over. Tx A inserts (lower id, older xid) and stays open; tx B inserts a
// higher id and commits. B is not visible until A finishes, and then both come
// out in id order.
func TestNoSkipEarlierOpenTx(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "x", 2) // m_x0, m_x1
	_, cur, _ := run(t, ctx, c, "")

	a, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx) //nolint:errcheck
	rawEvent(t, ctx, a, "m_x0", 1000, "EVIDENCE_RECORDED", "{}")
	rawEvents(t, st, ctx, "m_x1", [2]string{"EVIDENCE_RECORDED", "{}"}) // B: higher id, committed
	evs, cur2, _ := run(t, ctx, c, cur)
	if live := withoutLag(evs); len(live) != 0 {
		t.Fatalf("a row behind an open earlier transaction was exported: %v", idsOf(live))
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _, _ = run(t, ctx, c, cur2)
	ids := idsOf(evs)
	if len(ids) != 2 {
		t.Fatalf("after A commits want both rows, got %v", ids)
	}
	var a0, b0 int
	fmt.Sscanf(ids[0], "fabric:%d", &a0)
	fmt.Sscanf(ids[1], "fabric:%d", &b0)
	if a0 >= b0 {
		t.Fatalf("not in id order: %v", ids)
	}
}

// NEG-2, burst: concurrent writers through Execute while an exporter pulls in a
// loop; the union of everything pulled equals the outbox, with no repeat.
func TestNoSkipUnderConcurrentWriters(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	const writers, each = 8, 6
	var wg sync.WaitGroup
	stop := make(chan struct{})
	seen := map[string]bool{}
	cur := ""
	pull := func() {
		var buf bytes.Buffer
		if _, err := Export(ctx, c, cur, &buf); err != nil {
			t.Error(err)
			return
		}
		evs, next := parseOutput(t, buf.Bytes())
		for _, id := range idsOf(withoutLag(evs)) {
			if seen[id] {
				t.Errorf("%s repeated", id)
			}
			seen[id] = true
		}
		cur = next
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				pull()
			}
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id := domain.MissionID(fmt.Sprintf("m_w%d_%d", w, i))
				if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#1", "fabric", "main"); err != nil {
					t.Error(err)
					return
				}
				if _, err := st.Execute(ctx, "chat", domain.CommandID("admit:"+string(id)), domain.Command{Kind: domain.CmdAdmitMission, Mission: id}); err != nil {
					t.Errorf("admit %s: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-done
	pull() // quiescent: everything committed is now older than every open xid
	var total int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("no writes happened")
	}
	if len(seen) != total {
		t.Fatalf("exported %d of %d outbox rows: rows were skipped", len(seen), total)
	}
}

// AC-1 end to end: every line a real store produces validates against the
// pinned schema, carries a fabric: id, the event time and the mission as the
// stream, a summary of at most 280 characters, and none of the payload text.
func TestMapEventsFromStore(t *testing.T) {
	schema := loadSchema(t)
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "ae", 3)
	long := `{"note":"` + secret + strings.Repeat("y", 600) + `"}`
	rawEvents(t, st, ctx, "m_ae0", [2]string{"EVIDENCE_RECORDED", long}, [2]string{"DELIVERED", long})
	var buf bytes.Buffer
	if _, err := Export(ctx, c, "", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("payload text in the output:\n%s", buf.String())
	}
	evs, _ := parseOutput(t, buf.Bytes())
	if len(evs) != 5 {
		t.Fatalf("%d events, want 5", len(evs))
	}
	rows, err := st.Pool().Query(ctx, `SELECT o.id, o.mission_id, e.at FROM outbox o JOIN events e USING (mission_id, seq) ORDER BY o.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id int64
		var mission string
		var at time.Time
		if err := rows.Scan(&id, &mission, &at); err != nil {
			t.Fatal(err)
		}
		ev := evs[i]
		i++
		if bad := validate(schema, any(ev), "$"); len(bad) > 0 {
			t.Errorf("%v: %v", ev, bad)
		}
		if ev["id"] != fmt.Sprintf("fabric:%d", id) || ev["ts"] != at.UTC().Format("2006-01-02T15:04:05.000Z") ||
			ev["subject"].(map[string]any)["initiative"] != mission || ev["subject"].(map[string]any)["ticket"] != "goodqa#1" {
			t.Errorf("row %d mapped to %v", id, ev)
		}
	}
}

// Review F2: a store that has a row with the same id but other content (same
// database name and oid, rebuilt or a different cluster) must refuse the cursor.
func TestCursorBoundToRowContent(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "a", 3)
	_, cur, _ := run(t, ctx, c, "")
	if err := st.Truncate(ctx); err != nil {
		t.Fatal(err)
	}
	seed(t, st, ctx, "b", 3) // same row count and ids, different missions
	var buf bytes.Buffer
	_, err := Export(ctx, c, cur, &buf)
	if !errors.Is(err, ErrBadCursor) || !strings.Contains(err.Error(), "different content") || buf.Len() != 0 {
		t.Fatalf("err %v, %d bytes", err, buf.Len())
	}
}

// Review F1: rows held back by an older open transaction (here in another
// database of the cluster) are reported, so a stall is not read as caught up.
func TestLaggingReported(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "l", 1)
	_, cur, _ := run(t, ctx, c, "")
	blocker := conn(t, ctx, storetest.DSN)
	if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	var x string
	if err := blocker.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&x); err != nil {
		t.Fatal(err)
	}
	seed(t, st, ctx, "m", 2)
	evs, cur2, res := run(t, ctx, c, cur)
	if lag := lagEvents(evs); len(withoutLag(evs)) != 0 || len(lag) != 1 || cur2 != cur || res.Lagging < 2 || lag[0]["severity"] != "warn" || lag[0]["kind"] != "system" || lag[0]["summary"] != fmt.Sprintf("fabric export lagging: %d rows held back", res.Lagging) || !strings.HasPrefix(lag[0]["id"].(string), "fabric:lag:") {
		t.Fatalf("events=%d cursor same=%v lagging=%d", len(evs), cur2 == cur, res.Lagging)
	}
	// The id is keyed on the first held outbox row and stays the same while more rows pile up.
	var firstHeld int64
	_, curID, _, _, _ := decodeCursor(cur)
	if err := c.QueryRow(ctx, `SELECT min(id) FROM outbox WHERE id > $1`, curID).Scan(&firstHeld); err != nil {
		t.Fatal(err)
	}
	wantID := fmt.Sprintf("fabric:lag:%d", firstHeld)
	if got := lagEvents(evs)[0]["id"]; got != wantID {
		t.Fatalf("lag id %v, want %s (first held row)", got, wantID)
	}
	seed(t, st, ctx, "n", 1)
	evs2, _, res2 := run(t, ctx, c, cur)
	if lag := lagEvents(evs2); len(lag) != 1 || lag[0]["id"] != wantID || res2.Lagging <= res.Lagging {
		t.Fatalf("lag episode id not stable: %v lagging %d -> %d", lagEvents(evs2), res.Lagging, res2.Lagging)
	}
	if _, err := blocker.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	evs, _, res = run(t, ctx, c, cur)
	if len(evs) == 0 || res.Lagging != 0 || len(lagEvents(evs)) != 0 {
		t.Fatalf("after the blocker ended: events=%d lagging=%d", len(evs), res.Lagging)
	}
}

// A backfill without --since is historic; once caught up, new events are live.
func TestHistoricOnBackfillOnly(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "hb", 3)
	first, cur, res := run(t, ctx, c, "")
	if res.More || len(first) == 0 {
		t.Fatalf("setup: more=%v events=%d", res.More, len(first))
	}
	for _, e := range first {
		if e["historic"] != true {
			t.Fatalf("backfill event not historic: %v", e)
		}
	}
	seed(t, st, ctx, "hc", 1)
	next, _, _ := run(t, ctx, c, cur)
	if len(next) == 0 {
		t.Fatal("no new event")
	}
	for _, e := range next {
		if _, ok := e["historic"]; ok {
			t.Fatalf("live event marked historic: %v", e)
		}
	}
}

// Mission Control's join key: subject.ticket is the "project#N" reference of the
// mission's ticket and nothing else of the ticket text; no reference, no field.
func TestSubjectTicket(t *testing.T) {
	for in, want := range map[string]string{
		"GoodQA goodqa#14791":                    "goodqa#14791",
		"goodqa#15586 and more text":             "goodqa#15586",
		"GoodQA #5":                              "",
		"no reference " + secret:                 "",
		"x\x1b[31m goodqa#7\n":                   "goodqa#7",
		"goodqa#12345678901234567890":            "",
		"goodqa#1234567890":                      "",
		"goodqa#1234567":                         "goodqa#1234567",
		"goodqa#12345678":                        "",
		"abcdefghijklmnopqrstuvwxyzabcdefghij#5": "abcdefghijklmnopqrstuvwxyzabcdefghij#5",
		"\u212Aimi#5":                            "",
		"foo.goodqa#5":                           "",
		"my_proj#5":                              "",
		"GoodQA GOODQA#77.":                      "goodqa#77",
		"(goodqa#9)":                             "goodqa#9",
	} {
		if got := ticketRef(in); got != want {
			t.Errorf("ticketRef(%q) = %q, want %q", in, got, want)
		}
	}
	ev := MapRow(Row{ID: 1, Mission: "m_t", Seq: 1, Kind: "MISSION_ADMITTED", At: at, Ticket: "GoodQA goodqa#14791 " + secret})
	line, _ := json.Marshal(ev)
	if ev.Subject.Ticket != "goodqa#14791" || ev.Subject.Initiative != "m_t" || strings.Contains(string(line), secret) {
		t.Fatalf("%s", line)
	}
}

// Review F6/F8: distinct malformed mission ids stay distinct; a missing event record is named as such.
func TestMapMalformedAndOrphan(t *testing.T) {
	a := MapRow(Row{ID: 1, Mission: "bad id 1", Seq: 1, Kind: "MISSION_ADMITTED", At: at})
	b := MapRow(Row{ID: 2, Mission: "bad id 2", Seq: 1, Kind: "MISSION_ADMITTED", At: at})
	if a.Subject.Initiative == b.Subject.Initiative || a.Subject.Initiative == "invalid" {
		t.Fatalf("%q %q", a.Subject.Initiative, b.Subject.Initiative)
	}
	o := MapRow(Row{ID: 3, Mission: "m_o", Seq: 1, Kind: "", At: at})
	if o.Kind != "system" || !strings.Contains(o.Summary, "no event record") {
		t.Fatalf("%+v", o)
	}
}
