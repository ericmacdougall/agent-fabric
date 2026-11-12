package dispatch

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// AC-9 / NEG-11: the pin equals the stdin fake file.
func TestStdinFakePinned(t *testing.T) {
	got, err := FileSHA256(filepath.Join("..", "..", "tools", "fakeclaude", "fakeclaude-stdin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got != StdinFakeSHA256 {
		t.Fatalf("StdinFakeSHA256 is %s; the file hashes to %s (regenerate the constant after a reviewed change)", StdinFakeSHA256, got)
	}
}

var forbiddenImports = map[string]bool{"os/exec": true, "syscall": true, "golang.org/x/sys/unix": true}

// noExecProblems lists process-start routes in the non-test Go files of dir.
func noExecProblems(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if forbiddenImports[p] {
				out = append(out, filepath.Base(f)+": imports "+p)
			}
		}
		src, _ := os.ReadFile(f)
		if bytes.Contains(src, []byte("os.StartProcess")) {
			out = append(out, filepath.Base(f)+": calls os.StartProcess")
		}
	}
	return out
}

// AC-3: the dispatch package never starts a process; the host Launcher does.
func TestNoExec(t *testing.T) {
	if p := noExecProblems(t, "."); len(p) > 0 {
		t.Fatalf("dispatch must not start processes: %v", p)
	}
	// the check is not vacuous: a planted syscall import is caught
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "x.go"), []byte("package dispatch\nimport _ \"syscall\"\n"), 0o644)
	os.WriteFile(filepath.Join(d, "y.go"), []byte("package dispatch\nimport \"os\"\nvar _ = os.StartProcess\n"), 0o644)
	if p := noExecProblems(t, d); len(p) != 2 {
		t.Fatalf("the check missed a planted process start: %v", p)
	}
}

type named string

func (n named) Name() string                         { return string(n) }
func (named) Plan(host.Grant, Input) (Launch, error) { return Launch{}, nil }
func (named) Settle(io.Reader, int) (Settlement, error) {
	return Settlement{}, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(ClaudeCode{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ClaudeCode{}); err == nil {
		t.Error("a duplicate name was accepted")
	}
	for _, bad := range []Dispatcher{nil, named("Bad"), named("x"), named("has space")} {
		if err := r.Register(bad); err == nil {
			t.Errorf("Register(%v) accepted", bad)
		}
	}
	var np *ClaudeCode
	if err := r.Register(np); err == nil {
		t.Error("a typed nil was accepted")
	}
	if d, ok := r.Get("claude-code"); !ok || d.Name() != "claude-code" {
		t.Error("claude-code not found")
	}
	if _, ok := (*Registry)(nil).Get("claude-code"); ok {
		t.Error("a nil registry returned a dispatcher")
	}
	if got := r.Names(); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("Names = %v", got)
	}
}

func grant(t *testing.T) host.Grant {
	d := t.TempDir()
	return host.Grant{Run: "r_x_1", Mission: "m_x", Fence: 1, Model: "fake-model", Effort: "n/a", Rung: "R0", TierTableSHA: "t",
		ConfigSHA: "n/a", Profile: "P1", MaxTurns: 3, MaxBudgetUSD: 1, Wall: time.Minute, Worktree: d, RunDir: d, Token: "r_x_1-1-0123456789abcdef0123456789abcdef"}
}

// AC-6 / NEG-5: the plan carries the prompt on stdin only.
func TestPlanPromptOnStdinOnly(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	const canary = "PROMPT-CANARY-7f3a"
	pl, err := ClaudeCode{}.Plan(grant(t), Input{Launch: claude.Launch{Binary: bin}, Prompt: canary})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range pl.Argv {
		if strings.Contains(a, canary) {
			t.Fatalf("the prompt is in argv: %q", pl.Argv)
		}
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(pl.StdinBytes, &msg); err != nil || msg.Type != "user" || msg.Message.Content != canary {
		t.Fatalf("stdin is not one user message with the prompt: %s (%v)", pl.StdinBytes, err)
	}
}

// NEG-11 / review v6 F2: the pin is re-checked against the file each launch
// resolves, so a symlink repointed after start is refused.
func TestPlanRechecksProviderPin(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "good")
	other := filepath.Join(d, "other")
	os.WriteFile(good, []byte("#!/bin/sh\n# pinned\n"), 0o755)
	os.WriteFile(other, []byte("#!/bin/sh\n# swapped\n"), 0o755)
	pin, _ := FileSHA256(good)
	link := filepath.Join(d, "provider")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	c := ClaudeCode{PinSHA256: pin}
	in := Input{Launch: claude.Launch{Binary: link, Resolve: resolve}, Prompt: "p"}
	if _, err := c.Plan(grant(t), in); err != nil {
		t.Fatalf("the pinned provider was refused: %v", err)
	}
	os.Remove(link)
	os.Symlink(other, link)
	if _, err := c.Plan(grant(t), in); err == nil || !strings.Contains(err.Error(), "not the pinned stdin fake") {
		t.Fatalf("a repointed provider was planned: %v", err)
	}
}
