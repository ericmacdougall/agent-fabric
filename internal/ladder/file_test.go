package ladder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	stp := writeFile(t, dir, "state.json", fileTestState)
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

func TestStatusFromFilesRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	pol := writeFile(t, dir, "policy.json", fileTestPolicy)
	good := writeFile(t, dir, "state.json", fileTestState)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	big := writeFile(t, dir, "big.json", `{"task":"`+strings.Repeat("x", MaxInput)+`"}`)
	bad := writeFile(t, dir, "bad.json", `{"task":`)
	cases := map[string]string{"symlink": link, "oversize": big, "malformed": bad, "directory": dir, "missing": filepath.Join(dir, "nope")}
	for name, path := range cases {
		if _, err := StatusFromFiles(path, pol); !errors.Is(err, ErrInput) {
			t.Errorf("state %s: err = %v, want ErrInput", name, err)
		}
		if _, err := StatusFromFiles(good, path); !errors.Is(err, ErrInput) {
			t.Errorf("policy %s: err = %v, want ErrInput", name, err)
		}
	}
	shapes := map[string]string{
		"no schema":     `{"task":"t","events":[]}`,
		"schema 2":      `{"schema":2,"task":"t","events":[]}`,
		"no task":       `{"schema":1,"events":[]}`,
		"empty task":    `{"schema":1,"task":"","events":[]}`,
		"no events":     `{"schema":1,"task":"t"}`,
		"null events":   `{"schema":1,"task":"t","events":null}`,
		"object events": `{"schema":1,"task":"t","events":{}}`,
	}
	for name, body := range shapes {
		p := writeFile(t, dir, "shape.json", body)
		if _, err := StatusFromFiles(p, pol); !errors.Is(err, ErrInput) {
			t.Errorf("state %s: err = %v, want ErrInput", name, err)
		}
	}
	badPol := writeFile(t, dir, "badpol.json", `{"lowest_rung":"R0","failures_per_rung":0,"rungs":{"R0":{"writable":true}},"ladder_limits":{"max_total_review_usd":1}}`)
	if _, err := StatusFromFiles(good, badPol); !errors.Is(err, ErrPolicy) {
		t.Errorf("invalid policy: err = %v, want ErrPolicy", err)
	}
}
