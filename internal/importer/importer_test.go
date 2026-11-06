package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "imp")) }

func fixture(t *testing.T) Options {
	t.Helper()
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	return Options{SlotsPath: abs("testdata/active-slots.json"), QueuePath: abs("testdata/user-decisions-queue.json"), MainWAL: "/x/main.jsonl"}
}

// Two runs over the same files: the first admits up to the cap and refuses
// the rest, the second replays every admission and adds no event, mission or ask.
func TestImportIdempotent(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	o := fixture(t)
	sf, qf, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Run(ctx, st, nil, o, sf, qf)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Cap: 4, Admitted: 4, Refused: 1, Queued: 3, Asks: 2, Skipped: 1}
	if first != want {
		t.Fatalf("first run: %+v, want %+v", first, want)
	}
	counts := func() [3]int {
		var c [3]int
		for i, q := range []string{"SELECT count(*) FROM events", "SELECT count(*) FROM missions", "SELECT count(*) FROM asks"} {
			if err := st.Pool().QueryRow(ctx, q).Scan(&c[i]); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	before := counts()
	second, err := Run(ctx, st, nil, o, sf, qf)
	if err != nil {
		t.Fatal(err)
	}
	want2 := Result{Cap: 4, Replayed: 4, Refused: 1, Queued: 3, Asks: 2, Skipped: 1}
	if second != want2 {
		t.Fatalf("second run: %+v, want %+v", second, want2)
	}
	if after := counts(); after != before {
		t.Fatalf("second run changed (events, missions, asks) from %v to %v", before, after)
	}
	if held, _ := st.SlotsHeld(ctx); held != 4 {
		t.Fatalf("slots held %d, want 4 (the cap)", held)
	}

	rows, err := st.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tickets := map[string]string{}
	for _, r := range rows {
		tickets[string(r.ID)] = r.Ticket
	}
	for id, tk := range map[string]string{
		"m_slot1": "GoodQA goodqa#11", "m_slot2": "GoodQA goodqa#12", "m_slot3": "GoodQA goodqa#1", "m_slot4": "GoodQA demo#14",
		"m_q01": "GoodQA goodqa#101", "m_q02": "GoodQA goodqa#102", "m_q03": "GoodQA goodqa#1",
	} {
		if tickets[id] != tk {
			t.Errorf("%s ticket %q, want %q", id, tickets[id], tk)
		}
	}
	asks, err := st.ListAsks(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, a := range asks {
		src[a.ID] = a.Source
	}
	if src["w0123456789a"] != "wal:/x/main.jsonl#w0123456789a" {
		t.Errorf("wal-sourced ask: %q", src["w0123456789a"])
	}
	// The "none" ask id is not a WAL id: the ask is sourced from the file, not refused.
	if src["q_item02"] != "file:"+o.QueuePath {
		t.Errorf("file-sourced ask: %q (all: %v)", src["q_item02"], src)
	}
}

func TestImportRefusesBadInput(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	o := fixture(t)
	dir := t.TempDir()

	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(`{"items":["`+strings.Repeat("x", MaxFileBytes)+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(Options{SlotsPath: o.SlotsPath, QueuePath: big, MainWAL: o.MainWAL}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized queue file: %v", err)
	}
	if _, _, err := Load(Options{SlotsPath: o.SlotsPath, QueuePath: o.QueuePath, MainWAL: "/x/main wal.jsonl"}); err == nil {
		t.Fatal("a path with a space was accepted as a provenance source")
	}
	if _, _, err := Load(Options{SlotsPath: o.SlotsPath, QueuePath: o.QueuePath}); err == nil {
		t.Fatal("missing main WAL accepted")
	}
	sf, qf, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	sf.Slots = append(sf.Slots, sf.Slots[0])
	if _, err := Run(ctx, st, nil, o, sf, qf); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated slot number: %v", err)
	}
}

func TestTicketRefAndQueuedItem(t *testing.T) {
	for raw, want := range map[string]string{"GoodQA x#3": "GoodQA x#3", "#7": "GoodQA goodqa#7", "": "F", "14882": "F"} {
		if got := TicketRef(raw, "F"); got != want {
			t.Errorf("TicketRef(%q) = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string][2]string{
		`"a for #9 then #10"`:              {"a for #9 then #10", "#9"},
		`{"item":"b","ticket":"#4"}`:       {"b", "#4"},
		`{"name":"c #5","item":"ignored"}`: {"c #5", "#5"},
		`42`:                               {"42", ""},
	} {
		n, tk := QueuedItem([]byte(raw))
		if n != want[0] || tk != want[1] {
			t.Errorf("QueuedItem(%s) = %q,%q want %q", raw, n, tk, want)
		}
	}
}
