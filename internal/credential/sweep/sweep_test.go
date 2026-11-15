package sweep

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/credential"
	"fabric/internal/host"
)

func synthToken(t testing.TB, n int) []byte {
	t.Helper()
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := []byte("sk-" + "ant-oat01-")
	for len(out) < n {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		out = append(out, alpha[v.Int64()])
	}
	return out
}

func setup(t testing.TB, tok []byte) []byte {
	t.Helper()
	b, err := credential.NewBound(credential.NewSecret(credential.KindToken, "claude-oauth.v3", tok))
	if err != nil {
		t.Fatal(err)
	}
	in, err := credential.BuildSweepInput(b)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func total(r Report) int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// (b) every form under both normalisations, split by a newline at every
// offset and by a JSON \n escape.
func TestSweepEveryFormAndSplit(t *testing.T) {
	tok := synthToken(t, 64)
	in := setup(t, tok)
	for _, f := range credential.AllForms {
		enc := credential.Encode(f, tok)
		root := t.TempDir()
		write(t, filepath.Join(root, "a"), append(append([]byte("x "), enc...), " y"...), 0o600)
		r := Run(in, []string{root}, DefaultLimits, nil)
		if !r.Complete || total(r) != 1 {
			t.Fatalf("%s: %+v", f, r)
		}
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Fatalf("%s: not removed", f)
		}
	}
	for i := 1; i < len(tok); i++ {
		for _, sep := range []string{"\n", `\n`, " \r\n\t", `\r`} {
			root := t.TempDir()
			write(t, filepath.Join(root, "d", "f"), []byte("{\"t\":\""+string(tok[:i])+sep+string(tok[i:])+"\"}"), 0o600)
			r := Run(in, []string{root}, DefaultLimits, nil)
			if !r.Complete || total(r) != 1 {
				t.Fatalf("offset %d sep %q: %+v", i, sep, r)
			}
		}
	}
}

// (b) overlapping occurrences of a periodic credential, adjacent
// occurrences, a match straddling the 64 KiB read buffer, and a crafted
// fingerprint collision that the HMAC rejects.
func TestSweepOverlapAdjacentBoundaryCollision(t *testing.T) {
	period := []byte("abcdefghijklmnop") // 16-byte period
	tok := bytes.Repeat(period, 4)       // 64 bytes, periodic
	in := setup(t, tok)
	root := t.TempDir()
	// tok + one more period contains the raw token twice, overlapping.
	write(t, filepath.Join(root, "overlap"), bytes.Repeat(period, 5), 0o600)
	r := Run(in, []string{root}, DefaultLimits, nil)
	if r.Counts["claude-oauth:claude-oauth.v3:raw"] != 2 {
		t.Fatalf("overlap: %+v", r.Counts)
	}
	tok2 := synthToken(t, 100)
	in2 := setup(t, tok2)
	root = t.TempDir()
	write(t, filepath.Join(root, "adj"), append(append([]byte{}, tok2...), tok2...), 0o600)
	pad := bytes.Repeat([]byte{'.'}, 64<<10-50)
	write(t, filepath.Join(root, "straddle"), append(pad, tok2...), 0o600)
	r = Run(in2, []string{root}, DefaultLimits, nil)
	if r.Counts["claude-oauth:claude-oauth.v3:raw"] != 3 {
		t.Fatalf("adjacent+straddle: %+v", r.Counts)
	}
	// Collision: a window with the same fingerprint but different bytes.
	var si credential.SweepInput
	json.Unmarshal(in2, &si)
	raw := tok2
	target := credential.Fingerprint(raw, si.Base, 32)
	crafted := append([]byte(nil), raw...)
	found := false
	for tries := 0; tries < 1<<24 && !found; tries++ {
		v, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		x := v.Uint64()
		for k := 0; k < 8; k++ {
			crafted[24+k] = 'a' + byte(x>>(5*k))&15
		}
		if !bytes.Equal(crafted[:32], raw[:32]) && credential.Fingerprint(crafted, si.Base, 32) == target {
			found = true
		}
	}
	if !found {
		t.Skip("no collision found in budget")
	}
	root = t.TempDir()
	write(t, filepath.Join(root, "collide"), crafted, 0o600)
	r = Run(in2, []string{root}, DefaultLimits, nil)
	if r.Candidates == 0 || total(r) != 0 || !r.Complete {
		t.Fatalf("collision: candidates=%d counts=%v", r.Candidates, r.Counts)
	}
}

// (a) NEG-13: a worker 0700 subdirectory with a 0600 token file is found and
// removed; specials and hard links are counted, outside files untouched,
// and the sweep is incomplete.
func TestSweepNegatives(t *testing.T) {
	tok := synthToken(t, 108)
	in := setup(t, tok)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	write(t, victim, []byte("keep me"), 0o600)

	root := t.TempDir()
	write(t, filepath.Join(root, "priv", "tok"), tok, 0o600)
	os.Chmod(filepath.Join(root, "priv"), 0o700)
	r := Run(in, []string{root}, DefaultLimits, nil)
	if !r.Complete || total(r) == 0 {
		t.Fatalf("private subdir: %+v", r)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatal("private subdir not removed")
	}
	// A 0000 file is chmod u+r'd and read.
	root = t.TempDir()
	write(t, filepath.Join(root, "locked"), tok, 0o000)
	if r := Run(in, []string{root}, DefaultLimits, nil); !r.Complete || total(r) == 0 {
		t.Fatalf("unreadable own file: %+v", r)
	}

	cases := map[string]func(root string){
		"outward symlink": func(root string) { os.Symlink(victim, filepath.Join(root, "link")) },
		"dir symlink":     func(root string) { os.Symlink(outside, filepath.Join(root, "dlink")) },
		"fifo":            func(root string) { syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600) },
		"hard link":       func(root string) { os.Link(victim, filepath.Join(root, "hl")) },
	}
	for name, mk := range cases {
		root := t.TempDir()
		write(t, filepath.Join(root, "ok"), []byte("plain"), 0o600)
		mk(root)
		done := make(chan Report, 1)
		go func() { done <- Run(in, []string{root}, DefaultLimits, nil) }()
		var r Report
		select {
		case r = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: sweep blocked", name)
		}
		if r.Complete {
			t.Fatalf("%s: complete: %+v", name, r)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "keep me" {
			t.Fatalf("%s: outside file touched", name)
		}
		if _, err := os.Stat(filepath.Join(root, "ok")); err != nil {
			t.Fatalf("%s: incomplete sweep removed files", name)
		}
	}
}

// (a) a directory replaced by a symlink during the sweep is never followed.
func TestSweepMidSweepSwap(t *testing.T) {
	tok := synthToken(t, 108)
	in := setup(t, tok)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secretish"), tok, 0o600)
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "f"), []byte("x"), 0o600)
	write(t, filepath.Join(root, "b", "f"), []byte("y"), 0o600)
	swapped := false
	now := func() time.Time {
		if !swapped {
			if _, err := os.Stat(filepath.Join(root, "b")); err == nil {
				// Swap b for a link to outside once traversal has begun.
				os.RemoveAll(filepath.Join(root, "b"))
				os.Symlink(outside, filepath.Join(root, "b"))
				swapped = true
			}
		}
		return time.Now()
	}
	r := Run(in, []string{root}, DefaultLimits, now)
	if total(r) != 0 {
		t.Fatalf("followed the swapped link: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "secretish")); err != nil {
		t.Fatal("outside file removed")
	}
	if r.Complete {
		t.Fatalf("swap not detected: %+v", r)
	}
}

// (e) NEG-27: each limit makes the sweep incomplete.
func TestSweepLimits(t *testing.T) {
	tok := synthToken(t, 64)
	in := setup(t, tok)
	small := Limits{Entries: 5, Bytes: 1000, Wall: time.Minute, Confirmations: 3}

	root := t.TempDir()
	write(t, filepath.Join(root, "big"), bytes.Repeat([]byte{'z'}, 1001), 0o600)
	if r := Run(in, []string{root}, small, nil); r.Complete || !has(r, ReasonBytes) {
		t.Fatalf("bytes+1: %+v", r)
	}
	root = t.TempDir()
	write(t, filepath.Join(root, "exact"), bytes.Repeat([]byte{'z'}, 1000), 0o600)
	if r := Run(in, []string{root}, small, nil); !r.Complete {
		t.Fatalf("exact bytes: %+v", r)
	}
	root = t.TempDir()
	for i := 0; i < 6; i++ {
		write(t, filepath.Join(root, string(rune('a'+i))), []byte("x"), 0o600)
	}
	if r := Run(in, []string{root}, small, nil); r.Complete || !has(r, ReasonEntries) {
		t.Fatalf("entries+1: %+v", r)
	}
	root = t.TempDir()
	write(t, filepath.Join(root, "f"), []byte("x"), 0o600)
	t0 := time.Now()
	calls := 0
	clock := func() time.Time { calls++; return t0.Add(time.Duration(calls) * 61 * time.Second) }
	if r := Run(in, []string{root}, small, clock); r.Complete || !has(r, ReasonWall) {
		t.Fatalf("clock: %+v", r)
	}
	// Colliding windows past the confirmation cap: the token repeated
	// confirms once per occurrence.
	root = t.TempDir()
	write(t, filepath.Join(root, "many"), bytes.Repeat(append(tok, ' '), 4), 0o600)
	if r := Run(in, []string{root}, Limits{Entries: 100, Bytes: 1 << 20, Wall: time.Minute, Confirmations: 3}, nil); r.Complete || !has(r, ReasonConfirmations) {
		t.Fatalf("confirmations: %+v", r)
	}
	// An entry with L < W, and malformed stdin.
	var si credential.SweepInput
	json.Unmarshal(in, &si)
	si.Entries[0].Len = 31
	short, _ := json.Marshal(si)
	if r := Run(short, []string{t.TempDir()}, DefaultLimits, nil); r.Complete || !has(r, ReasonShortPattern) {
		t.Fatalf("short: %+v", r)
	}
	for _, bad := range [][]byte{nil, []byte("{"), []byte(`{"key":"x"}`), bytes.Repeat([]byte(" "), 70<<10)} {
		if r := Run(bad, []string{t.TempDir()}, DefaultLimits, nil); r.Complete || !has(r, ReasonInput) {
			t.Fatalf("malformed: %+v", r)
		}
	}
}

func has(r Report, reason string) bool {
	for _, x := range r.Reasons {
		if x == reason {
			return true
		}
	}
	return false
}

// (c) NEG-27: argv, env and stdin carry no synthetic credential byte in any
// policy form, and no GoodQA key or DSN entry; an unverified stop launches
// nothing.
func TestSweepFabdSideCarriesNoCredential(t *testing.T) {
	tok := synthToken(t, 108)
	gq := []byte(strings.Repeat("Q", 10) + "goodqakeysynthetic0123456789")
	dsn := []byte("dsnpasswordsynthetic99")
	b, err := credential.NewBound(credential.NewSecret(credential.KindToken, "claude-oauth.v3", tok),
		credential.NewSecret(credential.KindGoodQA, "1111111111111111", gq),
		credential.NewSecret(credential.KindDSN, "2222222222222222", dsn))
	if err != nil {
		t.Fatal(err)
	}
	in, err := credential.BuildSweepInput(b)
	if err != nil {
		t.Fatal(err)
	}
	var argv, env []string
	var stdin []byte
	launch := func(a, e []string, s []byte) ([]byte, int, error) {
		argv, env, stdin = a, e, s
		rep, _ := json.Marshal(Run(s, a[3:], DefaultLimits, nil))
		return rep, 0, nil
	}
	root := t.TempDir()
	r := Fabd(host.StopReceipt{Verified: true}, "/opt/fab/fab", []string{root}, in, launch)
	if !r.Complete {
		t.Fatalf("%+v", r)
	}
	captured := []byte(strings.Join(argv, "\x00") + "\x00" + strings.Join(env, "\x00") + "\x00" + string(stdin))
	red := b.Redact(nil, captured)
	delete(red.Counts, credential.LabelLiteral) // the public literal travels by design
	if red.Exposed() {
		t.Fatalf("sweep inputs carry a credential form: %v", credential.SortedLabels(red.Counts))
	}
	for _, e := range env {
		for _, bad := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "FABRIC_DSN", "GOODQA"} {
			if strings.Contains(e, bad) {
				t.Fatalf("env carries %s", bad)
			}
		}
	}
	if strings.Contains(string(stdin), "goodqa") || strings.Contains(string(stdin), "dsn-password") {
		t.Fatal("stdin carries a GoodQA or DSN entry")
	}
	launched := 0
	for _, rc := range []host.StopReceipt{{Verified: false}, {Verified: true, Survivors: []host.Process{{PID: 9}}}, {Verified: true, Escaped: []host.Process{{PID: 9}}}} {
		r := Fabd(rc, "/opt/fab/fab", []string{root}, in, func(a, e []string, s []byte) ([]byte, int, error) { launched++; return nil, 0, nil })
		if r.Complete || !has(r, ReasonStopUnverified) {
			t.Fatalf("unverified: %+v", r)
		}
	}
	if launched != 0 {
		t.Fatal("launched after an unverified stop")
	}
	for _, out := range [][]byte{[]byte("junk"), []byte(`{"complete":true,"reasons":["x"],"entries":{},"counts":{}}`)} {
		if r := Fabd(host.StopReceipt{Verified: true}, "f", []string{root}, in, func(a, e []string, s []byte) ([]byte, int, error) { return out, 0, nil }); r.Complete {
			t.Fatalf("malformed report accepted: %s", out)
		}
	}
	if r := Fabd(host.StopReceipt{Verified: true}, "f", []string{root}, in, func(a, e []string, s []byte) ([]byte, int, error) { return []byte("{}"), 1, nil }); r.Complete || !has(r, ReasonExit) {
		t.Fatal("non-zero exit accepted")
	}
}
