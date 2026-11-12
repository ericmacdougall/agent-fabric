package agent_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fabric/internal/agent"
	"fabric/internal/tools"
)

func shaOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type editFixture struct {
	wt   string
	file string
	ed   *tools.Edit
	wr   *tools.Write
	rd   *tools.Read
}

func newEditFixture(t *testing.T) *editFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &editFixture{wt: filepath.Join(base, "wt")}
	if err := os.MkdirAll(f.wt, 0o755); err != nil {
		t.Fatal(err)
	}
	f.file = filepath.Join(f.wt, "notes.txt")
	if err := os.WriteFile(f.file, []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin, err := tools.PinWorktree(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	f.rd, err = tools.NewRead(f.wt, 64<<10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := tools.OpenRunnerDir(filepath.Join(base, "state", "preimage"), 64, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.Close)
	keeper := &tools.PreimageKeeper{Dir: dir}
	f.ed, f.wr = tools.NewEdit(f.rd, pin, keeper), tools.NewWrite(f.rd, pin, keeper)
	return f
}

const editInput = `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"ALPHA"}]}`

// AC-4: a crash after MarkExecuting and before the write leaves the call
// INTERRUPTED and the file unchanged.
func TestCrashDuringEditBeforeWrite(t *testing.T) {
	f := newEditFixture(t)
	r := newRig(t, toolStep("", "", "toolu_edit_0001", "edit", editInput), endStep("fin"))
	r.cfgMod = func(c *agent.Config) { c.Tools = []agent.Tool{f.ed} }
	r.crashOnce("after_mark_executing")
	if _, _, died := r.life(); died == nil {
		t.Fatal("no crash")
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	c := r.calls()[0]
	if c.State != "INTERRUPTED" || !strings.Contains(string(c.Result), shaOf("alpha beta\n")) {
		t.Fatalf("%s %s", c.State, c.Result)
	}
	if got, _ := os.ReadFile(f.file); string(got) != "alpha beta\n" {
		t.Fatalf("file changed: %q", got)
	}
}

// AC-4: a crash after the rename and before DONE leaves it INTERRUPTED with the
// current sha256 (the post-image) next to the pre-image; the file is not edited
// twice and a leftover temp of that call is removed.
func TestCrashDuringEditAfterRename(t *testing.T) {
	f := newEditFixture(t)
	r := newRig(t, toolStep("", "", "toolu_edit_0001", "edit", editInput), endStep("fin"))
	r.cfgMod = func(c *agent.Config) { c.Tools = []agent.Tool{f.ed} }
	r.crashOnce("before_finish_tool")
	if _, _, died := r.life(); died == nil {
		t.Fatal("no crash")
	}
	if got, _ := os.ReadFile(f.file); string(got) != "ALPHA beta\n" {
		t.Fatalf("the edit did not land before the crash: %q", got)
	}
	tmp := filepath.Join(f.wt, ".fabric-edit0001.tmp")
	os.WriteFile(tmp, []byte("partial"), 0o644)
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	c := r.calls()[0]
	for _, s := range []string{shaOf("ALPHA beta\n"), shaOf("alpha beta\n")} {
		if c.State != "INTERRUPTED" || !strings.Contains(string(c.Result), s) {
			t.Fatalf("%s %s lacks %s", c.State, c.Result, s)
		}
	}
	if got, _ := os.ReadFile(f.file); string(got) != "ALPHA beta\n" {
		t.Fatalf("edited twice or lost: %q", got)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("a leftover temp of that call was not removed")
	}
}

// NEG-14: every edit, write and spill read leaves the loop's tool-call row with
// the redacted result; a refused edit records the refusal.
func TestEditAuditRecord(t *testing.T) {
	f := newEditFixture(t)
	r := newRig(t,
		toolStep("", "", "toolu_a1", "edit", `{"path":"notes.txt","edits":[{"oldText":"nothere","newText":"x"}]}`),
		toolStep("", "", "toolu_a2", "edit", editInput),
		toolStep("", "", "toolu_a3", "write", `{"path":"new/w.txt","content":"hello"}`),
		endStep("fin"))
	r.cfgMod = func(c *agent.Config) { c.Tools = []agent.Tool{f.ed, f.wr} }
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	cs := r.calls()
	if len(cs) != 3 {
		t.Fatalf("%d tool-call rows", len(cs))
	}
	if !cs[0].IsError || !strings.Contains(string(cs[0].Result), "matches nothing") || cs[0].State != "DONE" {
		t.Fatalf("a refused edit must record the refusal: %+v %s", cs[0].State, cs[0].Result)
	}
	for i, want := range []string{shaOf("alpha beta\n") + "", shaOf("hello")} {
		res := string(cs[i+1].Result)
		if cs[i+1].IsError || !strings.Contains(res, "pre_sha256:") || !strings.Contains(res, "post_sha256: "+map[int]string{0: shaOf("ALPHA beta\n"), 1: shaOf("hello")}[i]) {
			t.Fatalf("row %d lacks the digests: %s (%s)", i+1, res, want)
		}
	}
}

// NEG-10: a secret-shaped string in spilled output is redacted on read.
func TestSpillRedacted(t *testing.T) {
	f := newEditFixture(t)
	dir, err := tools.OpenRunnerDir(filepath.Join(filepath.Dir(f.wt), "state", "spill", "a"), 16, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	w, id, err := dir.Create("")
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("line one\nkey " + canary + "\nline three\n")
	w.Close()
	dir.Done(id, 10)
	f.rd.Spill = dir
	r := newRig(t, toolStep("", "", "toolu_r1", "read", `{"path":"spill:`+id+`","offset":1,"limit":10}`), endStep("fin"))
	r.cfgMod = func(c *agent.Config) { c.Tools = []agent.Tool{f.rd} }
	if out, err, _ := r.life(); err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("%v %s", err, r.kindOf(out))
	}
	res := string(r.calls()[0].Result)
	if strings.Contains(res, canary) || !strings.Contains(res, "[REDACTED]") || !strings.Contains(res, "line three") {
		t.Fatalf("spill read not redacted: %s", res)
	}
}
