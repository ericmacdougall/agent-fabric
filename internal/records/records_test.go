package records

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func rec(kind Kind, class string) Record {
	return Record{Mission: "m_canary", Attempt: "r_a1", Kind: kind, Class: class, Time: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
}

func TestAppendModeAndShape(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	if err := Append(dir, rec(DeliveryBlocked, "lease_budget")); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, Record{Mission: "m_canary", Attempt: "r_a1", Kind: LateWrite, CommentID: "c-123"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(Path(dir, "r_a1"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", di.Mode().Perm())
	}
	rs, err := Read(dir, "r_a1")
	if err != nil || len(rs) != 2 || rs[0].Class != "lease_budget" || rs[1].CommentID != "c-123" {
		t.Fatalf("read %+v %v", rs, err)
	}
	// The file is a JSON array of objects with only the AC-29 fields.
	raw, _ := os.ReadFile(Path(dir, "r_a1"))
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"mission": true, "attempt": true, "kind": true, "class": true, "time": true, "comment_id": true, "effect": true}
	for _, m := range arr {
		for k := range m {
			if !allowed[k] {
				t.Fatalf("unexpected field %q", k)
			}
		}
	}
}

func TestAppendIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := Append(dir, rec(DeliveredEarlier, "")); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := Read(dir, "r_a1")
	if len(rs) != 1 {
		t.Fatalf("%d records, want 1", len(rs))
	}
}

// AC-29: a record holds no body bytes or credential bytes. Free text in any
// field is refused, so a blocked body cannot be written through a record.
func TestRecordRefusesFreeText(t *testing.T) {
	dir := t.TempDir()
	body := "Fabric M1 result: here is the diagnosis sk-ant-oat01-SYNTHETIC"
	bad := []Record{
		rec(DeliveryBlocked, body),
		rec(DeliveryBlocked, ""),                                                      // delivery_blocked needs a class
		{Mission: "m_canary", Attempt: "r_a1", Kind: LateWrite},                       // needs comment id
		{Mission: "m_canary", Attempt: "r_a1", Kind: LateWrite, CommentID: body},      // free text
		{Mission: "m_canary", Attempt: "../r_a1", Kind: DeliveredEarlier},             // traversal
		{Mission: "m_canary", Attempt: "r_a1", Kind: "body"},                          // unknown kind
		{Mission: "m_canary", Attempt: "r_a1", Kind: DeliveredEarlier, Effect: "e_x"}, // bad effect id
	}
	for i, r := range bad {
		if err := Append(dir, r); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(raw), "sk-ant") || strings.Contains(string(raw), "diagnosis") {
			t.Fatalf("%s holds body bytes", e.Name())
		}
	}
}

func TestReadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(other, []byte("[]"), 0o600)
	os.Symlink(other, Path(dir, "r_a1"))
	if _, err := Read(dir, "r_a1"); err == nil {
		t.Fatal("followed a symlinked record file")
	}
	if err := Append(dir, rec(DeliveredEarlier, "")); err == nil {
		t.Fatal("appended through a symlinked record file")
	}
}

func appendMany(t *testing.T, dir string, n int) {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Append(dir, rec(DeliveryBlocked, fmt.Sprintf("class_%d", i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// Owner Q3: two (here many) concurrent Appends to the same attempt all survive.
func TestConcurrentAppendsAllSurvive(t *testing.T) {
	dir := t.TempDir()
	appendMany(t, dir, 32)
	rs, err := Read(dir, "r_a1")
	if err != nil || len(rs) != 32 {
		t.Fatalf("%d records survive, want 32 (%v)", len(rs), err)
	}
}

// Failing-first: without the lock the same test loses updates, so it is not
// vacuous.
func TestConcurrentAppendsWithoutLockLoseUpdates(t *testing.T) {
	old := lock
	lock = func(string) (func(), error) { return func() {}, nil }
	defer func() { lock = old }()
	lost := false
	for try := 0; try < 20 && !lost; try++ {
		dir := t.TempDir()
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); Append(dir, rec(DeliveryBlocked, fmt.Sprintf("class_%d", i))) }(i) //nolint:errcheck
		}
		wg.Wait()
		rs, _ := Read(dir, "r_a1")
		lost = len(rs) != 32
	}
	if !lost {
		t.Skip("could not provoke a lost update without the lock on this machine")
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	dir := t.TempDir()
	a := rec(DeliveryBlocked, "scan_token_form")
	a.Time = a.Time.Add(time.Minute)
	b := Record{Mission: "m_other", Attempt: "r_b", Kind: DeliveryBlocked, Class: "lease_budget", Time: a.Time.Add(-time.Hour)}
	c := Record{Mission: "m_other", Attempt: "r_b", Kind: DeliveredEarlier, Time: a.Time}
	for _, r := range []Record{a, b, c} {
		if err := Append(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := List(dir, DeliveryBlocked)
	if err != nil || len(rs) != 2 || rs[0].Attempt != "r_b" || rs[1].Attempt != "r_a1" {
		t.Fatalf("%+v %v", rs, err)
	}
}
