package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinWorkerOnlyExactProxy(t *testing.T) {
	p := PolicyDescriptor{Version: 1, DefaultDeny: true, ProxyURL: "http://127.0.0.1:31234", WorkerRules: []FirewallRule{{Action: "allow", Protocol: "tcp", Address: "127.0.0.1", Port: 31234}}}
	text, err := DarwinWorkerProfile(p)
	if err != nil || !strings.Contains(text, "(deny network*)") || !strings.Contains(text, `(remote tcp4 "localhost:31234")`) || strings.Contains(text, "remote ip") || strings.Contains(text, `(remote tcp `) || strings.Contains(text, "tcp6") {
		t.Fatalf("bad profile %v %s", err, text)
	}
	mutations := []func(*PolicyDescriptor){func(p *PolicyDescriptor) { p.DefaultDeny = false }, func(p *PolicyDescriptor) { p.WorkerRules[0].Address = "::1" }, func(p *PolicyDescriptor) { p.WorkerRules[0].Port = 0 }, func(p *PolicyDescriptor) { p.ProxyURL = "http://localhost:31234" }, func(p *PolicyDescriptor) { p.ProxyURL += "/other" }, func(p *PolicyDescriptor) { p.WorkerRules = append(p.WorkerRules, p.WorkerRules[0]) }}
	for _, mutate := range mutations {
		q := clonePolicy(p)
		mutate(&q)
		if _, err := DarwinWorkerProfile(q); err == nil {
			t.Fatal("unsafe worker policy compiled")
		}
	}
}

func TestDarwinCodexHistoryBindsUnaliasedRoots(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	base := "(version 1)\n(allow default)\n"
	profile, err := DarwinCodexHistory(base, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"sessions", "archived_sessions"} {
		path := filepath.Join(home, ".codex", root)
		if !strings.Contains(profile, `(deny file-read* (subpath "`+path+`"))`) {
			t.Fatal("history root unprotected")
		}
		if err := os.Symlink(t.TempDir(), path); err != nil {
			t.Fatal(err)
		}
		if _, err := DarwinCodexHistory(base, home); err == nil {
			t.Fatal("aliased history root accepted")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := DarwinCodexHistory(base, alias); err == nil {
		t.Fatal("aliased home accepted")
	}
	for _, invalid := range []string{"relative", "/", home + "/../other"} {
		if _, err := DarwinCodexHistory(base, invalid); err == nil {
			t.Fatal("invalid home accepted")
		}
	}
}

func TestDarwinPreservesLogsWithoutTouchingCredentialRefresh(t *testing.T) {
	p, err := DarwinPreserveLogs("(version 1)\n(allow default)\n", "/public/home")
	if err != nil || !strings.Contains(p, `(deny file-write-unlink (subpath "/public/home/.claude/projects"))`) || !strings.Contains(p, `/public/home/.agent-harness/codex-wals`) || strings.Contains(p, ".credentials") || strings.Contains(p, "auth.json") {
		t.Fatalf("unsafe preservation profile %v %s", err, p)
	}
	for _, path := range []string{".codex/log", ".codex/logs_2.sqlite", ".codex/logs_2.sqlite-wal", ".codex/logs_2.sqlite-shm", ".codex/logs_2.sqlite-journal"} {
		if !strings.Contains(p, `(deny file-write* (subpath "/public/home/`+path+`"))`) {
			t.Fatal("existing SQL log writes unprotected", path)
		}
	}
	for _, home := range []string{"/", "relative", "/public/../home", "/public/\"injected"} {
		if _, err := DarwinPreserveLogs("(version 1)\n", home); err == nil {
			t.Fatal("invalid home accepted")
		}
	}
}
