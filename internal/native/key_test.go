package native_test

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/native"
	"fabric/internal/provider/anthropic"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
)

// goodqa#15087 AC-5, NEG-5, NEG-9: the native key shape, its cache, the
// redaction of rotated keys, and the base_url allow-list.

const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

func randTail(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = keyAlphabet[r.Intn(len(keyAlphabet))]
	}
	return string(b)
}

// keyScript writes a key command that prints the contents of a value file
// and appends a line to a count file per run.
func keyScript(t *testing.T) (script, valueFile, countFile string) {
	t.Helper()
	d := t.TempDir()
	script, valueFile, countFile = filepath.Join(d, "key.sh"), filepath.Join(d, "value"), filepath.Join(d, "count")
	body := "#!/bin/sh\necho run >> '" + countFile + "'\ncat '" + valueFile + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, valueFile, countFile
}

func runs(t *testing.T, countFile string) int {
	t.Helper()
	b, err := os.ReadFile(countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run\n")
}

// leaks reports a 6-character substring of secret that appears in text.
func leaks(secret, text string) string {
	for i := 0; i+6 <= len(secret); i++ {
		if strings.Contains(text, secret[i:i+6]) {
			return secret[i : i+6]
		}
	}
	return ""
}

func TestKeyShape(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	stateDir(t)
	for _, c := range []struct{ what, value string }{
		{"OAuth token", "sk-ant-oat01-" + randTail(r, 100)},
		{"admin key", "sk-ant-admin01-" + randTail(r, 100)},
		{"other token", "sk-ant-ort01-" + randTail(r, 100)},
		{"API prefix, too short", "sk-ant-api03-" + randTail(r, 79)},
		{"API prefix, too long", "sk-ant-api03-" + randTail(r, 201)},
		{"API prefix, bad character", "sk-ant-api03-" + randTail(r, 50) + "." + randTail(r, 49)},
	} {
		t.Run(c.what, func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
			script, value, countFile := keyScript(t)
			if err := os.WriteFile(value, []byte(c.value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e.g.KeyCommand = []string{script}
			rec, err := native.Run(ctx, st, e.g, opts(true))
			if !errors.Is(err, anthropic.ErrKeyShape) || !strings.Contains(err.Error(), "want sk-ant-apiNN-") {
				t.Fatalf("seed %d: %s: %v, want ErrKeyShape naming the expected prefix", seed, c.what, err)
			}
			if runs(t, countFile) != 1 || len(e.srv.Requests()) != 0 || rec != nil {
				t.Fatalf("seed %d: key runs %d, requests %d, receipt %v; want 1, 0, nil", seed, runs(t, countFile), len(e.srv.Requests()), rec)
			}
			if n := count(t, st, "SELECT count(*) FROM attempts WHERE mission_id = $1", e.g.Mission); n != 0 {
				t.Fatalf("seed %d: %d attempt rows after a refused key", seed, n)
			}
			tail := c.value[13:]
			if s := leaks(tail, err.Error()); s != "" {
				t.Fatalf("seed %d: the error carries %q of the value", seed, s)
			}
			for _, dir := range []string{e.g.RunDir, e.g.Worktree} {
				filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error { //nolint:errcheck
					if err != nil || fi.IsDir() {
						return nil
					}
					b, _ := os.ReadFile(p)
					if s := leaks(tail, string(b)); s != "" {
						t.Errorf("seed %d: %s carries %q of the value", seed, p, s)
					}
					return nil
				})
			}
		})
	}
}

// A refused value is not cached: the next call runs the command again. An
// accepted value is cached for the TTL (control).
func TestKeyRefusalNotCached(t *testing.T) {
	ctx := context.Background()
	script, value, countFile := keyScript(t)
	src, _, err := native.NewKeyRing([]string{script}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(1))
	if err := os.WriteFile(value, []byte("sk-ant-oat01-"+randTail(r, 100)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := src(ctx); !errors.Is(err, anthropic.ErrKeyShape) {
			t.Fatalf("call %d: %v, want ErrKeyShape", i, err)
		}
		if n := runs(t, countFile); n != i {
			t.Fatalf("after refused call %d the command ran %d times, want %d (a refusal was cached)", i, n, i)
		}
	}
	good := "sk-ant-api03-" + randTail(r, 90)
	if err := os.WriteFile(value, []byte(good+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if k, err := src(ctx); err != nil || k != good {
			t.Fatalf("good key call %d: err %v", i, err)
		}
	}
	if n := runs(t, countFile); n != 3 {
		t.Fatalf("the command ran %d times, want 3 (two refusals, then one cached accept)", n)
	}
}

// Every value the source returns is redacted as a literal, so a key rotated
// mid-run is scrubbed too, even where the generic shape does not match.
func TestRotatedKeyRedacted(t *testing.T) {
	ctx := context.Background()
	script, value, _ := keyScript(t)
	src, red, err := native.NewKeyRing([]string{script}, 0) // ttl 0: every call refetches
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(2))
	a, b := "sk-ant-api03-"+randTail(r, 90), "sk-ant-api03-"+randTail(r, 90)
	for _, k := range []string{a, b} {
		if err := os.WriteFile(value, []byte(k+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := src(ctx); err != nil || got != k {
			t.Fatalf("fetch: err %v", err)
		}
	}
	// "x" before the key defeats the generic shape (and no "token=" label, which a named-credential rule would catch)'s word boundary, so only
	// the literal list can catch it.
	for _, k := range []string{a, b} {
		out := red.Redact("seen x" + k + " end")
		if out != "seen x[REDACTED:literal] end" {
			t.Fatalf("redacted %q; want the key replaced as a literal (leak %q)", out, leaks(k[13:], out))
		}
	}
}

// NEG-9: a grant whose base_url is not exactly https://api.anthropic.com is
// refused before the key command runs and before any write or request.
func TestBaseURLAllowList(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	for _, u := range []string{"http://127.0.0.1", "https://api.anthropic.com.evil.example", "https://api.anthropic.com/v1",
		"https://api.anthropic.com/", "http://api.anthropic.com", "https://api.anthropic.com:443", "https://user@api.anthropic.com", ""} {
		t.Run(u, func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 50, 5)})
			e.g.BaseURL = u
			cmds := count(t, st, "SELECT count(*) FROM commands")
			_, err := native.Run(ctx, st, e.g, opts(true))
			if err == nil || !strings.Contains(err.Error(), "base_url must be exactly https://api.anthropic.com") {
				t.Fatalf("base_url %q: %v, want the allow-list refusal", u, err)
			}
			if exists(e.marker) || len(e.srv.Requests()) != 0 || count(t, st, "SELECT count(*) FROM commands") != cmds {
				t.Fatalf("base_url %q: key read %v, %d requests; want no key, no request, no command", u, exists(e.marker), len(e.srv.Requests()))
			}
			if err := e.g.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
				t.Fatalf("Validate accepted base_url %q: %v", u, err)
			}
		})
	}
}
