//go:build darwin

package egress

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const historyMarker = "PUBLIC_HISTORY_FIXTURE_ONLY\n"

type historyReport struct {
	Reads         map[string]bool `json:"reads"`
	AuthCanary    bool            `json:"auth_canary"`
	Agents        bool            `json:"agents"`
	Hook          bool            `json:"hook"`
	SQLite        bool            `json:"sqlite"`
	CurrentAppend bool            `json:"current_append"`
}

// TestDarwinHistoryPublicBoundary runs only with a new explicit private fixture
// parent. It invokes no native provider, authentication or historical input.
func TestDarwinHistoryPublicBoundary(t *testing.T) {
	parent := os.Getenv("FABRIC_PUBLIC_HISTORY_ROOT")
	if parent == "" {
		t.Skip("public native history boundary probe not requested")
	}
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent {
		t.Fatal("fixture parent invalid")
	}
	st, err := os.Lstat(parent)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
		t.Fatal("fixture parent must be owned0700 real directory")
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		t.Fatal("fixture parent owner mismatch")
	}
	for _, wide := range []bool{false, true} {
		name := "strict"
		if wide {
			name = "widened"
		}
		t.Run(name, func(t *testing.T) {
			home, err := os.MkdirTemp(parent, name+"-")
			if err != nil {
				t.Fatal(err)
			}
			// Retain owned public evidence. No user log roots are used or removed.
			for _, root := range []string{"sessions", "archived_sessions"} {
				dir := filepath.Join(home, ".codex", root)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "public.jsonl"), []byte(historyMarker), 0600); err != nil {
					t.Fatal(err)
				}
				var compressed bytes.Buffer
				z := gzip.NewWriter(&compressed)
				_, _ = z.Write([]byte(historyMarker))
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "public.jsonl.gz"), compressed.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, filepath.Join(home, "alias_"+root)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "public.jsonl"), filepath.Join(home, "file_alias_"+root)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(home, ".codex/agent-harness"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{".codex/public-auth-canary", ".codex/AGENTS.md", ".codex/agent-harness/wal.py"} {
				if err := os.WriteFile(filepath.Join(home, path), []byte(historyMarker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			profile, err := DarwinWorkerProfile(PolicyDescriptor{Version: 1, DefaultDeny: true, ProxyURL: "http://127.0.0.1:65534", WorkerRules: []FirewallRule{{Action: "allow", Protocol: "tcp", Address: "127.0.0.1", Port: 65534}}})
			if err != nil {
				t.Fatal(err)
			}
			profile, err = DarwinPreserveLogs(profile, home)
			if err != nil {
				t.Fatal(err)
			}
			baseProfile := profile
			profile, err = DarwinCodexHistory(profile, home)
			if err != nil {
				t.Fatal(err)
			}
			if wide {
				// Deliberately omit only this boundary to reproduce prior authority.
				profile = baseProfile
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, os.Args[0], "-test.run=^TestDarwinHistoryChild$")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "FABRIC_HISTORY_CHILD=1", "GOMAXPROCS=1"}
			cmd.Dir = home
			out, err := cmd.Output()
			if err != nil || len(out) > 8192 {
				t.Fatal("public sandbox child failed")
			}
			var report historyReport
			if json.Unmarshal(out, &report) != nil || len(report.Reads) != 14 {
				t.Fatal("public report invalid")
			}
			for key, read := range report.Reads {
				if read != wide {
					t.Fatal("history boundary mismatch", key)
				}
			}
			if !report.AuthCanary || !report.Agents || !report.Hook || !report.SQLite || !report.CurrentAppend {
				t.Fatal("intended current inputs or writes blocked")
			}
			b, err := os.ReadFile(filepath.Join(home, ".codex/sessions/current-owned.jsonl"))
			if err != nil || string(b) != historyMarker {
				t.Fatal("current owned append not persisted")
			}
		})
	}
}

func TestDarwinHistoryChild(t *testing.T) {
	if os.Getenv("FABRIC_HISTORY_CHILD") != "1" {
		t.Skip("public child only")
	}
	home := os.Getenv("HOME")
	r := historyReport{Reads: map[string]bool{}}
	read := func(path string) bool { b, e := os.ReadFile(path); return e == nil && string(b) == historyMarker }
	for _, root := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(home, ".codex", root)
		_, e := os.ReadDir(dir)
		r.Reads[root+"_enumerate"] = e == nil
		r.Reads[root+"_raw"] = read(filepath.Join(dir, "public.jsonl"))
		b, e := os.ReadFile(filepath.Join(dir, "public.jsonl.gz"))
		if e == nil {
			z, e := gzip.NewReader(bytes.NewReader(b))
			if e == nil {
				data, e := io.ReadAll(io.LimitReader(z, 1024))
				r.Reads[root+"_compressed"] = e == nil && string(data) == historyMarker
				_ = z.Close()
			}
		}
		if _, ok := r.Reads[root+"_compressed"]; !ok {
			r.Reads[root+"_compressed"] = false
		}
		r.Reads[root+"_directory_alias"] = read(filepath.Join(home, "alias_"+root, "public.jsonl"))
		r.Reads[root+"_file_alias"] = read(filepath.Join(home, "file_alias_"+root))
		_, e = os.ReadDir(filepath.Join(home, "alias_"+root))
		r.Reads[root+"_alias_enumerate"] = e == nil
		out, e := exec.Command("/bin/cat", filepath.Join(dir, "public.jsonl")).Output()
		r.Reads[root+"_descendant"] = e == nil && string(out) == historyMarker
	}
	r.AuthCanary = read(filepath.Join(home, ".codex/public-auth-canary"))
	r.Agents = read(filepath.Join(home, ".codex/AGENTS.md"))
	r.Hook = read(filepath.Join(home, ".codex/agent-harness/wal.py"))
	out, e := exec.Command("/usr/bin/sqlite3", filepath.Join(home, "fresh.sqlite"), "CREATE TABLE fixture(v); INSERT INTO fixture VALUES(1); SELECT v FROM fixture;").Output()
	r.SQLite = e == nil && strings.TrimSpace(string(out)) == "1"
	f, e := os.OpenFile(filepath.Join(home, ".codex/sessions/current-owned.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e == nil {
		_, e = f.WriteString(historyMarker)
		r.CurrentAppend = e == nil && f.Close() == nil
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
	os.Exit(0)
}
