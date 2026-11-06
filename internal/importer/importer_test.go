package importer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

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
	want := Result{Cap: 4, FileCap: 4, Admitted: 4, Refused: 1, Queued: 3, Asks: 2, Skipped: 1}
	if !reflect.DeepEqual(first, want) {
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
	// Review F5: the replayed refusal is a replay, not a second refusal, and
	// the 5 slot specs, 3 queued specs and 2 asks are unchanged, not new.
	want2 := Result{Cap: 4, FileCap: 4, Replayed: 5, Unchanged: 10, Skipped: 1}
	if !reflect.DeepEqual(second, want2) {
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

// Review F5: a rerun over edited files reports each row whose stored text
// differs and leaves the stored text alone; nothing is silently ignored.
func TestImportReportsChangedText(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	o := fixture(t)
	sf, qf, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, st, nil, o, sf, qf); err != nil {
		t.Fatal(err)
	}
	sf.Slots[0].Name = "renamed slot"
	sf.QueuedInOrder[0] = []byte(`"a different queued item for #101"`)
	qf.Items[0].Need = "a different question"
	res, err := Run(ctx, st, nil, o, sf, qf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"m_q01", "m_slot1", "w0123456789a"}; !reflect.DeepEqual(res.Changed, want) {
		t.Fatalf("changed rows: %v, want %v (result %+v)", res.Changed, want, res)
	}
	if res.Unchanged != 7 || res.Queued != 0 || res.Asks != 0 {
		t.Fatalf("counts after an edited rerun: %+v", res)
	}
	if !strings.Contains(res.String(), "changed=3") {
		t.Fatalf("summary hides the changed rows: %s", res)
	}
	a, err := st.GetAsk(ctx, "w0123456789a")
	if err != nil || a.Question == "a different question" {
		t.Fatalf("the edited question was applied: %+v %v", a, err)
	}
}

// Review F5: the admission cap is global; an untrusted file proposes it and
// only an explicit ApplyCap (fab import --apply-cap) applies it.
func TestImportCapIsAdvisory(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	o := fixture(t)
	sf, qf, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	sf.Rules.MaxActiveOwners = 9
	res, err := Run(ctx, st, nil, o, sf, qf)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := st.SlotCap(ctx); c != 4 || res.Cap != 4 || res.FileCap != 9 || res.Admitted != 4 || res.Refused != 1 {
		t.Fatalf("file cap applied without ApplyCap: store cap %d, result %+v", c, res)
	}
	if !strings.Contains(res.String(), "not applied") {
		t.Fatalf("summary does not say the file cap was not applied: %s", res)
	}
	// A stored refusal replays under its key, so ApplyCap is shown on a fresh store.
	st = storetest.Open(t)
	o.ApplyCap = true
	res, err = Run(ctx, st, nil, o, sf, qf)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := st.SlotCap(ctx); c != 9 || res.Cap != 9 || res.Admitted != 5 || res.Refused != 0 {
		t.Fatalf("ApplyCap: store cap %d, result %+v", c, res)
	}
}

// Review F6: the importer reads only regular files. A FIFO would block the
// open forever (no writer); a directory or a device is not a queue file.
func TestImportRefusesNonRegularFiles(t *testing.T) {
	o := fixture(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "queue.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fifo, link, dir, "/dev/null"} {
		done := make(chan error, 1)
		go func() {
			_, _, err := Load(Options{SlotsPath: o.SlotsPath, QueuePath: p, MainWAL: o.MainWAL})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("%s: want a not-a-regular-file refusal, got %v", p, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Load blocked (a FIFO open waits for a writer)", p)
		}
	}
}
