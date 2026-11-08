package ladder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fileTestPolicy = `{"lowest_rung":"R0","failures_per_rung":2,
"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60},
"rungs":{"R0":{"writable":true,"needs_user_permission":false},"R1":{"writable":true,"needs_user_permission":false},
"R2":{"writable":true,"needs_user_permission":false}}}`

const fileTestState = `{"schema":1,"task":"synthetic-t1","launches":3,"spend_usd":1.5,"permission":null,
"source_note":"must never be printed",
"events":[
 {"class":"FAIL","fingerprint":"aaaaaaaaaaaaaaaaaaaa","head":"h1","source":"secret-source-text"},
 {"class":"FAIL","fingerprint":"aaaaaaaaaaaaaaaaaaaa","head":"h2"},
 {"class":"FAIL","fingerprint":"bbbbbbbbbbbbbbbbbbbb","head":"h3"}]}`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatusFromFilesGolden(t *testing.T) {
	dir := t.TempDir()
	stp := writeFile(t, dir, "synthetic-t1.json", fileTestState)
	pol := writeFile(t, dir, "policy.json", fileTestPolicy)
	st, err := StatusFromFiles(stp, pol)
	if err != nil {
		t.Fatal(err)
	}
	text, err := Format(st, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "task synthetic-t1\nrung R1\nfailures_at_rung 1\nclimbs 1\nfingerprint bbbbbbbbbbbb\nblocked_by -\nneeds_user_permission false\n"
	if text != want {
		t.Fatalf("got\n%s\nwant\n%s", text, want)
	}
	js, err := Format(st, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`"rung": "R1"`, `"climbs": 1`, `"blocked_by": []`} {
		if !strings.Contains(js, needle) {
			t.Errorf("json output lacks %s:\n%s", needle, js)
		}
	}
	for _, out := range []string{text, js} {
		if strings.Contains(out, "secret-source-text") || strings.Contains(out, "must never be printed") {
			t.Fatalf("output leaks receipt text:\n%s", out)
		}
	}
}

// TestStatusFromFilesParity sends every escalation.py vector through the
// file path too, so the strict decoder is held to the reference.
func TestStatusFromFilesParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var pf parityFile
	decodeStrict(t, raw, &pf)
	if len(pf.Vectors) < 50 {
		t.Fatalf("seed %d: only %d vectors", pf.Seed, len(pf.Vectors))
	}
	dir := t.TempDir()
	for _, v := range pf.Vectors {
		sub := filepath.Join(dir, strings.ReplaceAll(v.Name, "/", "_"))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		var st map[string]json.RawMessage
		decodeStrict(t, v.State, &st)
		st["schema"] = json.RawMessage("1")
		body, _ := json.Marshal(st)
		var task string
		decodeStrict(t, st["task"], &task)
		stp := writeFile(t, sub, task+".json", string(body))
		pol := writeFile(t, sub, "policy.json", string(pf.Policies[v.Policy]))
		got, err := StatusFromFiles(stp, pol)
		if err != nil {
			t.Fatalf("seed %d %s: %v", pf.Seed, v.Name, err)
		}
		if !reflect.DeepEqual(got, v.Want) {
			t.Errorf("seed %d %s: got %+v want %+v", pf.Seed, v.Name, got, v.Want)
		}
	}
}

func TestStatusFromFilesRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	pol := writeFile(t, dir, "policy.json", fileTestPolicy)
	good := writeFile(t, dir, "synthetic-t1.json", fileTestState)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	big := writeFile(t, dir, "big.json", `{"task":"`+strings.Repeat("x", MaxInput)+`"}`)
	bigPol := writeFile(t, dir, "bigpol.json", `{"x":"`+strings.Repeat("x", MaxPolicy)+`"}`)
	bad := writeFile(t, dir, "bad.json", `{"task":`)
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"symlink": link, "malformed": bad, "directory": dir, "missing": filepath.Join(dir, "nope"), "fifo": fifo}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for name, path := range cases {
			if _, err := StatusFromFiles(path, pol); !errors.Is(err, ErrInput) {
				t.Errorf("state %s: err = %v, want ErrInput", name, err)
			}
			if _, err := StatusFromFiles(good, path); !errors.Is(err, ErrInput) {
				t.Errorf("policy %s: err = %v, want ErrInput", name, err)
			}
		}
		if _, err := StatusFromFiles(big, pol); !errors.Is(err, ErrInput) {
			t.Errorf("oversize state: err = %v", err)
		}
		if _, err := StatusFromFiles(good, bigPol); !errors.Is(err, ErrInput) {
			t.Errorf("oversize policy: err = %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

// TestDecodeStateMatchesPythonStrictness covers inputs where Go's lenient
// decoder and Python's json disagree (S1A-RT-1..6): each is refused.
func TestDecodeStateMatchesPythonStrictness(t *testing.T) {
	ev := `[{"class":"FAIL","fingerprint":"a","head":"h1"}]`
	cases := map[string]string{
		"no schema":             `{"task":"t","events":[]}`,
		"schema 2":              `{"schema":2,"task":"t","events":[]}`,
		"schema string":         `{"schema":"1","task":"t","events":[]}`,
		"schema float":          `{"schema":1.0,"task":"t","events":[]}`,
		"no task":               `{"schema":1,"events":[]}`,
		"empty task":            `{"schema":1,"task":"","events":[]}`,
		"task not file stem":    `{"schema":1,"task":"other","events":[]}`,
		"task with newline":     `{"schema":1,"task":"t\nrung R0","events":[]}`,
		"no events":             `{"schema":1,"task":"t"}`,
		"null events":           `{"schema":1,"task":"t","events":null}`,
		"object events":         `{"schema":1,"task":"t","events":{}}`,
		"event not object":      `{"schema":1,"task":"t","events":[1]}`,
		"trailing value":        `{"schema":1,"task":"t","events":[]} {"schema":2}`,
		"trailing junk":         `{"schema":1,"task":"t","events":[]} junk`,
		"lone surrogate fp":     `{"schema":1,"task":"t","events":[{"class":"FAIL","fingerprint":"\ud800","head":"h1"}]}`,
		"replacement char head": `{"schema":1,"task":"t","events":[{"class":"FAIL","fingerprint":"a","head":"\ufffd"}]}`,
		"numeric fingerprint":   `{"schema":1,"task":"t","events":[{"class":"FAIL","fingerprint":7,"head":"h1"}]}`,
		"numeric class":         `{"schema":1,"task":"t","events":[{"class":1}]}`,
		"launches null":         `{"schema":1,"task":"t","events":[],"launches":null}`,
		"launches float":        `{"schema":1,"task":"t","events":[],"launches":3.0}`,
		"launches string":       `{"schema":1,"task":"t","events":[],"launches":"3"}`,
		"launches bool":         `{"schema":1,"task":"t","events":[],"launches":true}`,
		"spend null":            `{"schema":1,"task":"t","events":[],"spend_usd":null}`,
		"spend string":          `{"schema":1,"task":"t","events":[],"spend_usd":"100"}`,
		"spend infinite":        `{"schema":1,"task":"t","events":[],"spend_usd":1e400}`,
		"top-level array":       `[1]`,
		"top-level null":        `null`,
		"uppercase key ignored": `{"schema":1,"task":"t","Events":` + ev + `}`,
		"folded key ignored":    `{"schema":1,"task":"t","events":[],"\u017fchema":2}`,
	}
	for name, body := range cases {
		if name == "uppercase key ignored" {
			// Python sees no "events" key: corrupt. Go must not fold "Events" into it.
			if _, err := DecodeState("t.json", []byte(body)); !errors.Is(err, ErrInput) {
				t.Errorf("%s: err = %v, want ErrInput", name, err)
			}
			continue
		}
		if name == "folded key ignored" {
			// "ſchema" (U+017F) is a different key in Python; schema stays 1.
			if _, err := DecodeState("t.json", []byte(body)); err != nil {
				t.Errorf("%s: err = %v, want accepted", name, err)
			}
			continue
		}
		if _, err := DecodeState("t.json", []byte(body)); !errors.Is(err, ErrInput) {
			t.Errorf("%s: err = %v, want ErrInput", name, err)
		}
	}
	if _, err := DecodeState("t.json", []byte{'{', '"', 's', 0xff, '"', ':', '1', '}'}); !errors.Is(err, ErrInput) {
		t.Errorf("invalid UTF-8: err = %v", err)
	}
	// Uppercase event keys are ignored like Python: CLASS is not class.
	st, err := DecodeState("t.json", []byte(`{"schema":1,"task":"t","events":[{"CLASS":"FAIL","fingerprint":"a","head":"h1"},{"CLASS":"FAIL","fingerprint":"a","head":"h2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Replay(testPolicy(), st.Events)
	if err != nil || got.Rung != "R0" || got.Climbs != 0 {
		t.Fatalf("CLASS key counted: %+v %v", got, err)
	}
	// Duplicate keys: the last wins, as in Python.
	st, err = DecodeState("t.json", []byte(`{"schema":1,"task":"t","events":[{"class":"FAIL"}],"events":[]}`))
	if err != nil || len(st.Events) != 0 {
		t.Fatalf("duplicate key: %+v %v", st, err)
	}
}

func TestDecodeErrorsNeverEchoContent(t *testing.T) {
	for _, body := range []string{
		`{"schema":1,"task":"t","events":[],"spend_usd":"sk-live-XXXX"}`,
		`{"schema":"sk-live-XXXX","task":"t","events":[]}`,
		`{"schema":1,"task":"sk-live-XXXX!","events":[]}`,
	} {
		_, err := DecodeState("t.json", []byte(body))
		if err == nil || strings.Contains(err.Error(), "sk-live") {
			t.Errorf("error %v echoes content or accepts it", err)
		}
	}
	_, err := DecodePolicy("p.json", []byte(`{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":"sk-live-XXXX"}}`))
	if err == nil || strings.Contains(err.Error(), "sk-live") {
		t.Errorf("policy error %v echoes content or accepts it", err)
	}
}

func TestDecodePolicyRefuses(t *testing.T) {
	base := map[string]string{
		"missing needs_user_permission": `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"missing writable":              `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"writable not bool":             `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":1,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"no ladder_limits":              `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false}}}`,
		"usd as string":                 `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":"60"}}`,
		"failures zero":                 `{"lowest_rung":"R0","failures_per_rung":0,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"failures float":                `{"lowest_rung":"R0","failures_per_rung":2.0,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"bad rung name":                 `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"r0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"undefined lowest":              `{"lowest_rung":"R9","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
		"uppercase key":                 `{"Lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false}},"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}}`,
	}
	for name, body := range base {
		if _, err := DecodePolicy("p.json", []byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStatusIntFloatCompareIsExact(t *testing.T) {
	p := testPolicy()
	p.Limits.MaxTotalReviewUSD = "9007199254740993" // 2**53 + 1, a Python int
	st, err := Status(p, LedgerState{SpendUSD: "9007199254740992.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.BlockedBy) != 0 {
		t.Fatalf("blocked_by %v; Python compares int 2**53+1 > float 2**53 exactly", st.BlockedBy)
	}
}

func TestFormatNeverPrintsRawFingerprint(t *testing.T) {
	odd := "a\x1b[2Jb\u00e9"
	out, err := Format(LadderStatus{Computed: Computed{Rung: "R0", Fingerprint: &odd}, Task: "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "fingerprint sha256:") {
		t.Fatalf("unsafe fingerprint output: %q", out)
	}
}
