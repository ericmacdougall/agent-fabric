package tui

import (
	"context"
	"errors"
	"fabric/internal/connref"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fabric/internal/metrics"
	"fabric/internal/storetest"
)

var update = flag.Bool("update", false, "rewrite testdata golden frames")

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "tui")) }

// fixedNow is the database clock every golden frame is drawn at.
var fixedNow = time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)

var testMetrics = metrics.Options{Version: "v-test", DispatchLimit: 8,
	Caps: []metrics.Cap{{Scope: "month", Micros: 100_000_000}, {Scope: "job", Micros: 5_000_000}}}

// open empties the test database and returns the storetest pool (a
// read-write admin path for seeding) and a TUI session on its own
// read-only pool.
func open(t *testing.T, timeout time.Duration) (*pgxpool.Pool, *Session) {
	t.Helper()
	st := storetest.Open(t)
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM sched_counters`); err != nil {
		t.Fatal(err)
	}
	pool, err := OpenPool(storetest.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return st.Pool(), &Session{Src: &Source{Pool: pool, Timeout: timeout, Metrics: testMetrics},
		M: Model{JobCap: 5_000_000}, Clock: func() time.Time { return fixedNow }}
}

// exec seeds in one transaction with triggers off, so rows can be written
// directly in any state.
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

func seed(t *testing.T, pool *pgxpool.Pool) {
	exec(t, pool,
		`INSERT INTO missions (id, ticket, execution, terminal, state, created_at, provider) VALUES
			('m_1', 'goodqa#1 build the thing', 'RUNNING', 'NONE', '{}', '2026-01-01T00:00:00Z', 'claude-code'),
			('m_2', 'goodqa#2 write the docs', 'IDLE', 'NONE', '{}', '2026-01-01T00:05:00Z', 'claude-code'),
			('m_3', 'goodqa#3 shipped', 'IDLE', 'SUCCEEDED', '{}', '2025-12-30T00:00:00Z', 'claude-code')`,
		`INSERT INTO attempts (id, mission_id, fence, revision, generation, state, lease_until, path) VALUES
			('a_1', 'm_1', 1, 0, 0, 'RUNNING', '2100-01-01T00:00:00Z', 'claude-code'),
			('a_2', 'm_2', 1, 0, 0, 'LEASED', '2026-01-01T00:07:00Z', 'claude-code'),
			('a_3', 'm_3', 1, 0, 0, 'ENDED', NULL, 'claude-code')`,
		`INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, dispatches, created_at, updated_at) VALUES
			('e_1', 'm_1', 'a_1', 1, 'ticket_comment', 's1', 'k1', '{}', 'x', 'CONFIRMED', 1, '2026-01-01T00:00:30Z', '2026-01-01T00:01:00Z'),
			('e_2', 'm_1', 'a_1', 1, 'publish', 's2', 'k2', '{}', 'x', 'QUARANTINED', 1, '2026-01-01T00:00:40Z', '2026-01-01T00:02:00Z'),
			('e_3', 'm_1', 'a_1', 1, 'ticket_comment', 's3', 'k3', '{}', 'x', 'UNKNOWN', 1, '2026-01-01T00:00:50Z', '2026-01-01T00:08:00Z'),
			('e_4', 'm_2', 'a_2', 1, 'publish', 's4', 'k4', '{}', 'x', 'RECONCILING', 2, '2026-01-01T00:06:00Z', '2026-01-01T00:09:00Z')`,
		`INSERT INTO effect_resolutions (effect_id, from_state, to_state, reason, evidence, actor, db_user, xact, at) VALUES
			('e_1', 'QUARANTINED', 'RESOLVED_APPLIED', 'r', 'ev', 'operator', 'u', 1, '2026-01-01T00:03:00Z')`,
		`INSERT INTO asks (id, mission_id, kind, question, source, state, created_at) VALUES
			('k_1', 'm_1', 'decision', 'ship it?', 'user', 'open', '2026-01-01T00:02:00Z'),
			('k_2', NULL, 'permission', 'allow network?', 'user', 'open', '2026-01-01T00:09:00Z')`,
		`INSERT INTO asks (id, mission_id, kind, question, source, state, answer, answered_by, answered_at, answer_source, created_at) VALUES
			('k_3', 'm_1', 'decision', 'which branch?', 'user', 'answered', 'main', 'user', now(), 'user', '2026-01-01T00:01:00Z')`,
		`INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha, cost_micros) VALUES
			('a_1', 1, 'assistant', '\x00', 1, 'x', 250000)`,
	)
}

// refresh runs one refresh and pins the database clock for golden frames.
func refresh(t *testing.T, s *Session) {
	t.Helper()
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.M.Data.Now = fixedNow
}

var plain = Opts{KeysOn: true}

func frame(m *Model, w, h int, o Opts) string {
	return strings.Join(Render(m, w, h, fixedNow, o), "\n") + "\n"
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func checkWidth(t *testing.T, lines []string, w int) {
	t.Helper()
	for i, l := range lines {
		if n := cells(stripSGR(l)); n > w {
			t.Fatalf("line %d is %d columns wide (> %d): %q", i, n, w, l)
		}
	}
}

// cells is an independent display width (not the package's runeWidth, so
// a wrong width table cannot hide an overflow): Han, Hangul, kana and
// full-width forms take 2 columns, combining marks 0.
func cells(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) || (r >= 0xff01 && r <= 0xff60),
			unicode.Is(unicode.So, r) && r >= 0x2300: // symbols terminals may draw as emoji (review F1 on ff48df1)
			n += 2
		default:
			n++
		}
	}
	return n
}

var sgrRE = regexp.MustCompile("\x1b\\[(?:0|1|7|31|33)m")

func stripSGR(s string) string { return sgrRE.ReplaceAllString(s, "") }

func key(c KeyCode) Key { return Key{Code: c} }
func char(r rune) Key   { return Key{Code: KeyRune, Rune: r} }
func press(m *Model, keysOn bool, ks ...Key) (res KeyResult) {
	for _, k := range ks {
		if r := m.Handle(k, keysOn); r.Refresh {
			res.Refresh = true
		}
	}
	return res
}

// AC-7: the frame for a seeded store matches testdata, pane by pane:
// header gauges, ordering, columns and the focused-row marker.
func TestGoldenFrame(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	var all strings.Builder
	for p := Pane(0); p < paneCount; p++ {
		s.M.Pane = p
		lines := Render(&s.M, 80, 24, fixedNow, plain)
		if len(lines) != 24 {
			t.Fatalf("%d lines", len(lines))
		}
		checkWidth(t, lines, 80)
		all.WriteString(strings.Join(lines, "\n") + "\n")
	}
	golden(t, "frame80x24.txt", all.String())
	got := all.String()
	// Spot checks so a regenerated golden cannot silently drop the brief.
	for _, want := range []string{"missions 2/3 active", "attempts 1 live 1 stale", "effects 1 unk 1 quar 1 rec",
		"asks 2", "month $0.25/$100.00", "$0.25/$5.00", "goodqa#1 build the th", "live 27027d", "stale 3m", "dispatch 0/8 0 late"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	// Effects: oldest first (e_2 updated 00:02, e_3 00:08, e_4 00:09);
	// CONFIRMED e_1 is not shown.
	if i2, i3, i4 := strings.Index(got, "e_2"), strings.Index(got, "e_3"), strings.Index(got, "e_4"); !(i2 < i3 && i3 < i4) || strings.Contains(got, "e_1 ") {
		t.Errorf("effects order or filter wrong")
	}
	// Missions: active first; the terminal m_3 last.
	if strings.Index(got, "m_3") < strings.Index(got, "m_2") {
		t.Errorf("terminal mission listed before active ones")
	}
}

// AC-7: Enter on a mission opens its attempts, effects, asks and decisions
// (decision asks plus effect resolutions); Esc returns.
func TestDrillDown(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	if !press(&s.M, true, key(KeyEnter)).Refresh || s.M.DetailID != "m_1" {
		t.Fatalf("Enter on the first mission: detail %q", s.M.DetailID)
	}
	if !strings.Contains(frame(&s.M, 80, 24, plain), "loading mission m_1") {
		t.Fatalf("no loading line before the detail arrives")
	}
	refresh(t, s)
	got := frame(&s.M, 80, 24, plain)
	golden(t, "detail80x24.txt", got)
	for _, want := range []string{"Attempts (1)", "Effects (3)", "Asks (2)", "Decisions (3)",
		"e_1  QUARANTINED -> RESOLVED_APPLIED  by operator", "ask k_1  open", "ask k_3  answered"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	press(&s.M, true, key(KeyEsc))
	if s.M.DetailID != "" || !strings.Contains(frame(&s.M, 80, 24, plain), "[Missions 3]") {
		t.Fatalf("Esc did not return to the list")
	}
	// Enter on an effect drills into its mission.
	s.M.Pane = PaneEffects
	s.M.Sel[PaneEffects] = 2 // e_4 of m_2
	press(&s.M, true, key(KeyEnter))
	if s.M.DetailID != "m_2" {
		t.Fatalf("Enter on e_4 opened %q", s.M.DetailID)
	}
	// A mission that disappears shows "not found", not a crash.
	s.M.DetailID = "m_gone"
	refresh(t, s)
	if !strings.Contains(frame(&s.M, 80, 24, plain), "mission m_gone not found") {
		t.Fatalf("missing mission not reported")
	}
}

// AC-7: arrows and j/k move, Tab cycles, / filters, Esc clears, q quits.
func TestNavigation(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	m := &s.M
	press(m, true, char('j'), key(KeyDown), key(KeyDown), key(KeyDown))
	if m.Sel[PaneMissions] != 2 {
		t.Fatalf("down past the end: sel %d", m.Sel[PaneMissions])
	}
	press(m, true, char('k'), key(KeyUp), key(KeyUp))
	if m.Sel[PaneMissions] != 0 {
		t.Fatalf("up past the top: sel %d", m.Sel[PaneMissions])
	}
	press(m, true, key(KeyTab))
	if m.Pane != PaneAttempts {
		t.Fatalf("Tab: pane %d", m.Pane)
	}
	press(m, true, key(KeyBackTab), key(KeyBackTab))
	if m.Pane != PaneAsks {
		t.Fatalf("Shift-Tab: pane %d", m.Pane)
	}
	m.Pane = PaneMissions
	press(m, true, char('/'), char('d'), char('o'), char('c'), char('q'), key(KeyBackspace), key(KeyEnter))
	if m.Quit || m.Filter != "doc" || len(m.rowsOf(PaneMissions)) != 1 {
		t.Fatalf("filter: quit=%v filter=%q rows=%d", m.Quit, m.Filter, len(m.rowsOf(PaneMissions)))
	}
	if f := frame(m, 80, 24, plain); !strings.Contains(f, "[filter: doc]") || strings.Contains(f, "m_1 ") {
		t.Fatalf("filtered frame:\n%s", f)
	}
	press(m, true, key(KeyEsc))
	if m.Filter != "" || len(m.rowsOf(PaneMissions)) != 3 {
		t.Fatalf("Esc did not clear the filter")
	}
	press(m, true, char('q'))
	if !m.Quit {
		t.Fatalf("q did not quit")
	}
}

// AC-7/AC-8: the focused row carries "> " in every mode and reverse video
// with colour, so focus never depends on colour alone.
func TestFocusVisible(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	s.M.Sel[PaneMissions] = 1
	lines := Render(&s.M, 80, 24, fixedNow, plain)
	if !strings.HasPrefix(lines[6], "> m_2") || strings.HasPrefix(lines[5], ">") {
		t.Fatalf("focus marker: %q / %q", lines[5], lines[6])
	}
	col := Render(&s.M, 80, 24, fixedNow, Opts{Color: true, KeysOn: true})
	if !strings.HasPrefix(col[6], sgrReverse+"> m_2") || !strings.HasSuffix(col[6], sgrReset) {
		t.Fatalf("colour focus: %q", col[6])
	}
	s.M.DetailID = "m_1"
	refresh(t, s)
	s.M.DetailSel = 2
	if d := Render(&s.M, 80, 24, fixedNow, plain); !strings.HasPrefix(d[5], "> Attempts") {
		t.Fatalf("detail focus: %q", d[2:6])
	}
}

// AC-8: with --keys=off, j, k, q and / do nothing; the arrows, Tab,
// Enter, Esc, Ctrl-F and Ctrl-C still work.
func TestKeysOff(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	m := &s.M
	press(m, false, char('j'), char('/'), char('q'), char('k'))
	if m.Sel[PaneMissions] != 0 || m.Filtering || m.Quit {
		t.Fatalf("single-key shortcut acted with keys off: %+v", m)
	}
	press(m, false, key(KeyDown))
	if m.Sel[PaneMissions] != 1 {
		t.Fatalf("Down with keys off")
	}
	press(m, false, key(KeyCtrlF), char('s'), char('h'), char('i'), key(KeyEnter))
	if m.Filter != "shi" || len(m.rowsOf(PaneMissions)) != 1 {
		t.Fatalf("Ctrl-F filter with keys off: %q", m.Filter)
	}
	press(m, false, key(KeyEsc), key(KeyTab))
	if m.Filter != "" || m.Pane != PaneAttempts {
		t.Fatalf("Esc/Tab with keys off")
	}
	m.Pane = PaneMissions
	if !press(m, false, key(KeyEnter)).Refresh {
		t.Fatalf("Enter with keys off")
	}
	if !strings.Contains(frame(m, 80, 24, Opts{}), "Ctrl-F filter  Ctrl-C quit") {
		t.Fatalf("keys-off help line not shown")
	}
	press(m, false, key(KeyCtrlC))
	if !m.Quit {
		t.Fatalf("Ctrl-C with keys off")
	}
	// The raw bytes decode as expected (arrows are not three typed runes).
	ks := ParseKeys([]byte("\x1b[A\x1b[Bj\x1b\x7f\x06\x03\x1b[Z\x1b[5~\x1b[9q"))
	want := []KeyCode{KeyUp, KeyDown, KeyRune, KeyEsc, KeyBackspace, KeyCtrlF, KeyCtrlC, KeyBackTab, KeyPgUp, KeyUnknown}
	if len(ks) != len(want) {
		t.Fatalf("ParseKeys: %+v", ks)
	}
	for i := range want {
		if ks[i].Code != want[i] {
			t.Fatalf("ParseKeys[%d] = %v, want %v", i, ks[i].Code, want[i])
		}
	}
	if k := ParseKeys([]byte("\x1b")); len(k) != 1 || k[0].Code != KeyEsc {
		t.Fatalf("lone ESC: %+v", k)
	}
}

// failFrames renders a frame with a disconnected and with a stale banner.
func failFrames(t *testing.T, s *Session, o Opts) (disc, stale []string) {
	t.Helper()
	s.M.Fail = &Failure{Disconnected: true}
	disc = Render(&s.M, 80, 24, fixedNow.Add(12*time.Second), o)
	s.M.Fail = &Failure{Code: "55P03"}
	stale = Render(&s.M, 80, 24, fixedNow.Add(3*time.Second), o)
	s.M.Fail = nil
	return disc, stale
}

// AC-8: --no-color frames carry no escape byte at all, and every cue is
// text: "> " focus, "!" with "disconnected" or "stale".
func TestNoColor(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	disc, stale := failFrames(t, s, Opts{KeysOn: true})
	for _, f := range [][]string{disc, stale, Render(&s.M, 80, 24, fixedNow, plain)} {
		if j := strings.Join(f, "\n"); strings.ContainsRune(j, 0x1b) {
			t.Fatalf("escape in a no-colour frame: %q", j)
		}
	}
	if !strings.HasPrefix(disc[0], "! disconnected: retrying (no answer; data 12s old)") {
		t.Fatalf("disconnected banner: %q", disc[0])
	}
	if !strings.HasPrefix(stale[0], "! stale: refresh failed (SQLSTATE 55P03; data 3s old)") {
		t.Fatalf("stale banner: %q", stale[0])
	}
	if !strings.HasPrefix(disc[6], "  m_2") || !strings.HasPrefix(disc[5], "> m_1") {
		t.Fatalf("data and focus not kept under the banner: %q", disc[5])
	}
}

// AC-8: a colour frame uses only bold, reverse, red and yellow, puts bold
// on the warning with its text cue, and never dim (2), grey (90) or white
// (37) text.
func TestColorFrame(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	col := Opts{Color: true, KeysOn: true}
	disc, stale := failFrames(t, s, col)
	if !strings.HasPrefix(disc[0], sgrBold+sgrRed+"! disconnected") || !strings.HasPrefix(stale[0], sgrBold+sgrYellow+"! stale") {
		t.Fatalf("warning banners: %q / %q", disc[0], stale[0])
	}
	for _, f := range [][]string{disc, stale, Render(&s.M, 80, 24, fixedNow, col)} {
		j := strings.Join(f, "\n")
		for _, bad := range []string{"\x1b[2m", "\x1b[90m", "\x1b[37m"} {
			if strings.Contains(j, bad) {
				t.Fatalf("forbidden SGR %q", bad)
			}
		}
		if rest := stripSGR(j); strings.ContainsRune(rest, 0x1b) {
			t.Fatalf("escape other than the allowed SGR set: %q", rest)
		}
	}
}

// hostile is runner-writable text built to attack the terminal: CSI colour
// and clear, OSC 8 hyperlink, OSC 52 clipboard write, BEL, a C1 CSI, DEL,
// a bidi override and an isolate.
const hostile = "evil\x1b[2J\x1b[31mRED\x1b]8;;http://x.invalid\x07link\x1b]8;;\x07\x1b]52;c;ZXZpbA==\x07\u009b6n\x7f\u202eesrever\u2066z\u2069"

// NEG-7: hostile ticket and question text renders inert in the lists and
// the detail, with and without colour.
func TestHostileTextInert(t *testing.T) {
	admin, s := open(t, 0)
	q := strings.ReplaceAll(hostile, "'", "''")
	exec(t, admin,
		`INSERT INTO missions (id, ticket, execution, terminal, state, provider) VALUES ('m_h', '`+q+`', 'RUNNING', 'NONE', '{}', 'claude-code')`,
		`INSERT INTO asks (id, mission_id, kind, question, source, state) VALUES ('k_h', 'm_h', 'decision', '`+q+`', 'user', 'open')`)
	refresh(t, s)
	var frames []string
	for p := Pane(0); p < paneCount; p++ {
		s.M.Pane = p
		frames = append(frames, frame(&s.M, 200, 24, plain), stripSGR(frame(&s.M, 200, 24, Opts{Color: true})))
	}
	s.M.Pane = PaneMissions
	press(&s.M, true, key(KeyEnter))
	refresh(t, s)
	frames = append(frames, frame(&s.M, 200, 24, plain), stripSGR(frame(&s.M, 200, 24, Opts{Color: true})))
	seen := 0
	for _, f := range frames {
		for _, l := range strings.Split(strings.TrimSuffix(f, "\n"), "\n") {
			assertInert(t, "frame line", l)
		}
		if strings.Contains(f, "evil") {
			seen++
		}
	}
	if seen < 3 { // missions pane, asks pane and detail must each show the text
		t.Fatalf("hostile text shown in only %d frames; the test would be vacuous", seen)
	}
}

// NEG-10: no line is wider than the terminal at 80, 40 and 20 columns, with
// 10,000-character and full-width fields; below the minimum a single
// "terminal too small" line is drawn.
func TestNarrowFrame(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	refresh(t, s)
	for _, w := range []int{80, 40, 20} {
		for p := Pane(0); p < paneCount; p++ {
			s.M.Pane = p
			for _, o := range []Opts{plain, {Color: true}} {
				lines := Render(&s.M, w, 12, fixedNow, o)
				checkWidth(t, lines, w)
				if len(lines) != 12 {
					t.Fatalf("w=%d: %d lines", w, len(lines))
				}
			}
		}
	}
	at40 := frame(&s.M, 40, 12, plain)
	if !strings.Contains(at40, "k_1") {
		t.Fatalf("40 columns lost the id column:\n%s", at40)
	}
	for _, sz := range [][2]int{{19, 24}, {80, 5}, {3, 3}} {
		lines := Render(&s.M, sz[0], sz[1], fixedNow, plain)
		if len(lines) != sz[1] || !strings.HasPrefix("terminal too small", strings.TrimSuffix(lines[0], "~")) {
			t.Fatalf("%v: %q", sz, lines)
		}
		checkWidth(t, lines, sz[0])
	}
	if l := Render(&s.M, 0, 0, fixedNow, plain); len(l) != 0 {
		t.Fatalf("0x0: %q", l)
	}
}

// NEG-10: very long and full-width fields are cut to the terminal.
func TestLongFields(t *testing.T) {
	m := &Model{Data: &Data{Now: fixedNow,
		Missions: []Mission{{ID: strings.Repeat("i", 10000), Ticket: strings.Repeat("漢", 10000), Execution: "RUNNING", Terminal: "NONE", Created: fixedNow},
			{ID: "m_emoji", Ticket: strings.Repeat("\u2705\u2614\u231a\u26a1\u2b50\U0001f600", 2000), Execution: "IDLE", Terminal: "NONE", Created: fixedNow}},
		Asks: []Ask{{ID: "k", Kind: "decision", Question: strings.Repeat("問", 10000) + strings.Repeat("x", 10000), Created: fixedNow}}},
		JobCap: 1}
	for _, w := range []int{80, 41, 40, 21, 20} {
		for p := Pane(0); p < paneCount; p++ {
			m.Pane = p
			checkWidth(t, Render(m, w, 10, fixedNow, plain), w)
			checkWidth(t, Render(m, w, 10, fixedNow, Opts{Color: true}), w)
		}
	}
	m.DetailID = m.Data.Missions[0].ID
	m.Detail = &Detail{Found: true, Mission: m.Data.Missions[0], Asks: m.Data.Asks}
	for _, w := range []int{80, 21, 20} {
		checkWidth(t, Render(m, w, 10, fixedNow, plain), w)
	}
}

// guardThrowaway refuses to touch any database but this process's own
// storetest one.
func guardThrowaway(t *testing.T, admin *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := admin.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "fabric_tui_") {
		t.Fatalf("refusing: %q is not this test's throwaway database", name)
	}
	return name
}

// NEG-8: the database refuses connections mid-session: the frame shows
// "disconnected" over the last data, and the next refresh after it comes
// back clears the banner.
func TestDisconnectRecovers(t *testing.T) {
	seedPool, s := open(t, time.Second)
	seed(t, seedPool)
	name := guardThrowaway(t, seedPool)
	ctx := context.Background()
	adminRef, err := connref.ResolveEnv("FABRIC_TEST_DSN", connref.TestAdmin)
	if err != nil {
		t.Fatal("test admin credential reference unavailable")
	}
	t.Cleanup(func() { adminRef.Close() })
	adminDSN, err := adminRef.DSN()
	if err != nil || adminDSN == "" {
		t.Fatal("test admin credential reference is required")
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close) // after the allow(true) cleanup below (LIFO)
	allow := func(on bool) {
		t.Helper()
		v := "false"
		if on {
			v = "true"
		}
		if _, err := admin.Exec(ctx, `ALTER DATABASE "`+name+`" ALLOW_CONNECTIONS `+v); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { allow(true) })
	refresh(t, s)

	allow(false)
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx); err == nil {
		t.Fatalf("refresh succeeded with connections refused")
	}
	if s.M.Fail == nil || !s.M.Fail.Disconnected {
		t.Fatalf("failure not shown as disconnected: %+v", s.M.Fail)
	}
	f := frame(&s.M, 80, 24, plain)
	if !strings.Contains(f, "! disconnected: retrying") || !strings.Contains(f, "m_1") {
		t.Fatalf("disconnected frame:\n%s", f)
	}
	if err := s.Refresh(ctx); err == nil { // still down: still disconnected
		t.Fatalf("second refresh succeeded")
	}

	allow(true)
	refresh(t, s)
	if f := frame(&s.M, 80, 24, plain); s.M.Fail != nil || strings.Contains(f, "disconnected") {
		t.Fatalf("banner not cleared after recovery:\n%s", f)
	}
}

// NEG-9: a held row lock never delays a refresh; under ACCESS EXCLUSIVE a
// refresh gives up at its own timeout and shows stale data, not a hang.
func TestRefreshUnderLocks(t *testing.T) {
	admin, s := open(t, 500*time.Millisecond)
	seed(t, admin)
	ctx := context.Background()
	refresh(t, s)

	rowLock, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rowLock.Exec(ctx, `SELECT 1 FROM effects WHERE id = 'e_2' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Refresh(ctx); err != nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("refresh under a row lock: %v after %s", err, time.Since(start))
	}
	rowLock.Rollback(ctx) //nolint:errcheck

	tableLock, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tableLock.Rollback(ctx) //nolint:errcheck
	if _, err := tableLock.Exec(ctx, `LOCK TABLE effects IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	s.Src.Timeout = 200 * time.Millisecond
	start = time.Now()
	err = s.Refresh(ctx)
	if d := time.Since(start); err == nil || d > 1200*time.Millisecond {
		t.Fatalf("refresh under ACCESS EXCLUSIVE: %v after %s", err, d)
	}
	// lock_timeout (55P03) or statement_timeout (57014), whichever fires
	// first: both mean the database answered and this refresh is stale.
	if s.M.Fail == nil || s.M.Fail.Disconnected || (s.M.Fail.Code != "55P03" && s.M.Fail.Code != "57014") {
		t.Fatalf("lock timeout shown as %+v", s.M.Fail)
	}
	if f := frame(&s.M, 80, 24, plain); !strings.Contains(f, "! stale: refresh failed (SQLSTATE "+s.M.Fail.Code) || !strings.Contains(f, "m_1") {
		t.Fatalf("stale frame:\n%s", f)
	}
}

// NEG-11 (a): the TUI's own pool is read-only: the real refresh and
// drill-down succeed on it, and a write through it fails with SQLSTATE
// 25006 even outside metrics.ReadOnly.
func TestReadOnlyPool(t *testing.T) {
	admin, s := open(t, 0)
	seed(t, admin)
	s.M.DetailID = "m_1"
	refresh(t, s)
	if s.M.Detail == nil || !s.M.Detail.Found || len(s.M.Data.Missions) != 3 {
		t.Fatalf("refresh on the read-only pool: %+v", s.M.Detail)
	}
	ctx := context.Background()
	_, err := s.Src.Pool.Exec(ctx, `UPDATE missions SET execution = 'PAUSED' WHERE id = 'm_1'`)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "25006" {
		t.Fatalf("write through the TUI pool: %v", err)
	}
	var ro string
	if err := s.Src.Pool.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&ro); err != nil || ro != "on" {
		t.Fatalf("default_transaction_read_only = %q (%v)", ro, err)
	}
}

// NEG-12: no frame shows a secret, the DSN, a prompt body or a path, and
// a connection failure's banner is fixed text plus SQLSTATE and age.
func TestNoSecretsInFrames(t *testing.T) {
	admin, s := open(t, time.Second)
	seed(t, admin)
	exec(t, admin,
		`INSERT INTO agent_messages (attempt_id, idx, role, content, fence, request_sha, cost_micros) VALUES
			('a_1', 2, 'user', convert_to('PROMPT-SENTINEL body', 'UTF8'), 1, NULL, 1)`,
		`INSERT INTO effects (id, mission_id, attempt_id, fence, kind, subject, idem_key, request, request_sha, state, dispatches) VALUES
			('e_s', 'm_1', 'a_1', 1, 'publish', '/srv/fabric/root/PATH-SENTINEL', 'ks', '{"path": "/srv/PATH-SENTINEL", "token": "TOKEN-SENTINEL"}', 'x', 'UNKNOWN', 1)`,
		`INSERT INTO asks (id, mission_id, kind, question, options, recommended, source, state) VALUES
			('k_s', 'm_1', 'credential-link', 'link the account', '["SECRET-SENTINEL"]', 'SECRET-SENTINEL', 'user', 'open')`)
	refresh(t, s)
	var frames []string
	for p := Pane(0); p < paneCount; p++ {
		s.M.Pane = p
		frames = append(frames, frame(&s.M, 200, 40, plain))
	}
	s.M.Pane = PaneMissions
	press(&s.M, true, key(KeyEnter))
	refresh(t, s)
	frames = append(frames, frame(&s.M, 200, 40, plain))

	// Swap in a pool whose DSN carries sentinels and cannot connect.
	bad, err := OpenPool("postgres://sentineluser:PW-SENTINEL@127.0.0.1:1/sentineldb?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	s.Src.Pool = bad
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("control: the driver error should name the host (else the check below is vacuous): %v", err)
	}
	if s.M.Fail == nil || !s.M.Fail.Disconnected {
		t.Fatalf("unreachable database not disconnected: %+v", s.M.Fail)
	}
	banner := Render(&s.M, 200, 40, fixedNow.Add(5*time.Second), plain)
	frames = append(frames, strings.Join(banner, "\n"))
	if !strings.HasPrefix(banner[0], "! disconnected: retrying (no answer; data 5s old)") &&
		!strings.HasPrefix(banner[0], "! disconnected: retrying (SQLSTATE ") {
		t.Fatalf("banner is not the fixed message: %q", banner[0])
	}
	shown := 0
	for _, f := range frames {
		for _, bad := range []string{"PROMPT-SENTINEL", "PATH-SENTINEL", "TOKEN-SENTINEL", "SECRET-SENTINEL", "PW-SENTINEL",
			"sentineluser", "sentineldb", "127.0.0.1", "/srv/"} {
			if strings.Contains(f, bad) {
				t.Fatalf("frame shows %q:\n%s", bad, f)
			}
		}
		if strings.Contains(f, "e_s") || strings.Contains(f, "k_s") {
			shown++
		}
	}
	if shown < 3 { // the sentinel rows themselves must be on screen
		t.Fatalf("sentinel rows shown in only %d frames; the test would be vacuous", shown)
	}
}
