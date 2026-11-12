package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"fabric/internal/tools"
)

var spillRe = regexp.MustCompile(`spill:([0-9a-f]{16})`)

func (f *fixture) spillSetup(t *testing.T) (*tools.Shell, *tools.Read, *tools.RunnerDir) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(f.wt), "state", "spill", "att1")
	rd, err := tools.OpenRunnerDir(dir, 16, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rd.Close)
	f.conf.Spill = rd
	sh := &tools.Shell{C: f.conf, Allow: []*regexp.Regexp{regexp.MustCompile(`.*`)}, Timeout: 20 * time.Second, MaxOutput: 32 << 10}
	r, err := tools.NewRead(f.wt, 64<<10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.Spill = rd
	return sh, r, rd
}

func shellIn(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func readIn(path string, offset, limit int) json.RawMessage {
	m := map[string]any{"path": path}
	if offset > 0 {
		m["offset"] = offset
	}
	if limit > 0 {
		m["limit"] = limit
	}
	b, _ := json.Marshal(m)
	return b
}

func TestSpillAndReadOffset(t *testing.T) {
	f := newFixture(t)
	sh, rd, dir := f.spillSetup(t)
	write(t, filepath.Join(f.wt, "gen.sh"), "i=1\nwhile [ $i -le 20000 ]; do echo line-$i; i=$((i+1)); done\n")
	out, isErr := sh.Run(context.Background(), shellIn("sh gen.sh"))
	if isErr {
		t.Fatal(out)
	}
	m := spillRe.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "[output truncated at 32768 bytes of ") {
		t.Fatalf("no spill notice: %q", out[len(out)-200:])
	}
	if len(out) > 32768+400 {
		t.Fatalf("output not capped: %d", len(out))
	}
	st, err := os.Stat(dir.Path())
	if err != nil || st.Mode().Perm() != 0o700 || strings.HasPrefix(dir.Path(), f.run) {
		t.Fatalf("spill dir mode/location wrong: %v %v", st, err)
	}
	got, isErr := rd.Run(context.Background(), readIn("spill:"+m[1], 1000, 50))
	if isErr {
		t.Fatal(got)
	}
	var want []string
	for i := 1000; i < 1050; i++ {
		want = append(want, fmt.Sprintf("line-%d", i))
	}
	if g := strings.TrimSuffix(got, "\n"); !strings.HasPrefix(g, strings.Join(want, "\n")) {
		t.Fatalf("window mismatch: %.120q", got)
	}
	if !strings.Contains(got, "continue with offset 1050") {
		t.Fatalf("no continuation hint: %q", got[len(got)-80:])
	}
	// the notice's own offset reads on from the cut
	if _, isErr := rd.Run(context.Background(), readIn("spill:"+m[1], 0, 0)); isErr {
		t.Fatal("whole-spill read refused")
	}
}

func TestSpillExactMaxOutputLeavesNothing(t *testing.T) {
	f := newFixture(t)
	sh, _, dir := f.spillSetup(t)
	write(t, filepath.Join(f.wt, "exact.sh"), "head -c 32768 /dev/zero | tr '\\0' a\n")
	out, isErr := sh.Run(context.Background(), shellIn("sh exact.sh"))
	if isErr || strings.Contains(out, "truncated") || strings.Contains(out, "spill:") || len(out) != 32768 {
		t.Fatalf("exact-size output reported as cut: err=%v len=%d", isErr, len(out))
	}
	write(t, filepath.Join(f.wt, "plus1.sh"), "head -c 32769 /dev/zero | tr '\\0' a\n")
	out, _ = sh.Run(context.Background(), shellIn("sh plus1.sh"))
	if !spillRe.MatchString(out) {
		t.Fatal("N+1 bytes must spill")
	}
	ents, _ := os.ReadDir(dir.Path())
	if len(ents) != 1 {
		t.Fatalf("%d spill files", len(ents))
	}
}

func TestSpillEvictsOldestAndReadsExpired(t *testing.T) {
	f := newFixture(t)
	sh, rd, _ := f.spillSetup(t)
	write(t, filepath.Join(f.wt, "big.sh"), "head -c 40000 /dev/zero | tr '\\0' b\n")
	var ids []string
	for i := 0; i < 17; i++ {
		out, _ := sh.Run(context.Background(), shellIn("sh big.sh"))
		m := spillRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatal("no spill")
		}
		ids = append(ids, m[1])
	}
	if out, isErr := rd.Run(context.Background(), readIn("spill:"+ids[0], 0, 0)); !isErr || !strings.Contains(out, "spill expired") {
		t.Fatalf("oldest spill still readable: %v %.80s", isErr, out)
	}
	if out, isErr := rd.Run(context.Background(), readIn("spill:"+ids[1], 0, 0)); isErr {
		t.Fatalf("second spill lost: %s", out)
	}
}

func TestSpillRefusals(t *testing.T) {
	f := newFixture(t)
	sh, rd, _ := f.spillSetup(t)
	write(t, filepath.Join(f.wt, "big.sh"), "head -c 40000 /dev/zero | tr '\\0' b\n")
	out, _ := sh.Run(context.Background(), shellIn("sh big.sh"))
	id := spillRe.FindStringSubmatch(out)[1]
	other, err := tools.OpenRunnerDir(filepath.Join(filepath.Dir(f.wt), "state", "spill", "att2"), 16, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	fo, name, _ := other.Create("")
	fo.WriteString("other attempt")
	fo.Close()
	other.Done(name, 13)
	for _, p := range []string{"spill:../../etc/passwd", "spill:ABC", "spill:" + id + "a", "spill:" + strings.ToUpper(id), "spill:" + name, "spill:", "spill:" + id + "/../" + id} {
		if o, isErr := rd.Run(context.Background(), readIn(p, 0, 0)); !isErr {
			t.Fatalf("%q accepted: %.60s", p, o)
		}
	}
	for _, bad := range []json.RawMessage{readIn("spill:"+id, 0, 2001), json.RawMessage(`{"path":"spill:` + id + `","offset":0}`)} {
		if o, isErr := rd.Run(context.Background(), bad); !isErr {
			t.Fatalf("bad window accepted: %.60s", o)
		}
	}
}

func TestSpillPlantedLinks(t *testing.T) {
	f := newFixture(t)
	sh, rd, dir := f.spillSetup(t)
	// the child plants links where a runner that trusted RunDir would write
	os.Symlink(f.outside, filepath.Join(f.run, "spill"))
	os.Symlink(filepath.Join(f.outside, "x"), filepath.Join(f.run, "tool-out-planted"))
	write(t, filepath.Join(f.wt, "big.sh"), "head -c 40000 /dev/zero | tr '\\0' b\n")
	out, _ := sh.Run(context.Background(), shellIn("sh big.sh"))
	m := spillRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no spill")
	}
	if ents, _ := os.ReadDir(f.outside); len(ents) != 0 {
		t.Fatalf("the runner wrote through a planted link: %v", ents)
	}
	// a spill replaced by a symlink, and one with a second link, are refused on read
	p := filepath.Join(dir.Path(), m[1])
	secret := filepath.Join(f.outside, "secret")
	os.WriteFile(secret, []byte("secret"), 0o600)
	os.Remove(p)
	os.Symlink(secret, p)
	if o, isErr := rd.Run(context.Background(), readIn("spill:"+m[1], 0, 0)); !isErr || strings.Contains(o, "secret") {
		t.Fatalf("symlinked spill read: %v %q", isErr, o)
	}
	out, _ = sh.Run(context.Background(), shellIn("sh big.sh"))
	m = spillRe.FindStringSubmatch(out)
	p = filepath.Join(dir.Path(), m[1])
	if err := os.Link(p, filepath.Join(f.outside, "hard")); err != nil {
		t.Fatal(err)
	}
	if o, isErr := rd.Run(context.Background(), readIn("spill:"+m[1], 0, 0)); !isErr {
		t.Fatalf("hard-linked spill read: %.60s", o)
	}
}

func TestSpillIdAfterResume(t *testing.T) {
	f := newFixture(t)
	sh, _, dir := f.spillSetup(t)
	write(t, filepath.Join(f.wt, "a.sh"), "head -c 40000 /dev/zero | tr '\\0' a\n")
	write(t, filepath.Join(f.wt, "b.sh"), "head -c 40000 /dev/zero | tr '\\0' b\n")
	out, _ := sh.Run(context.Background(), shellIn("sh a.sh"))
	idA := spillRe.FindStringSubmatch(out)[1]
	path := dir.Path()
	dir.Close()
	// a resumed attempt reopens the same directory
	dir2, err := tools.OpenRunnerDir(path, 16, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer dir2.Close()
	f.conf.Spill = dir2
	out, _ = sh.Run(context.Background(), shellIn("sh b.sh"))
	idB := spillRe.FindStringSubmatch(out)[1]
	if idA == idB {
		t.Fatal("a new spill reused an id")
	}
	r2, _ := tools.NewRead(f.wt, 1<<20, time.Second)
	r2.Spill = dir2
	a, _ := r2.Run(context.Background(), readIn("spill:"+idA, 0, 0))
	b, _ := r2.Run(context.Background(), readIn("spill:"+idB, 0, 0))
	if !strings.HasPrefix(a, "aaaa") || !strings.HasPrefix(b, "bbbb") {
		t.Fatalf("spills do not read back their own bytes: %.10q %.10q", a, b)
	}
}

func TestSpillOrphanReap(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"running", "dead1", "dead2"} {
		os.MkdirAll(filepath.Join(root, n), 0o700)
		os.WriteFile(filepath.Join(root, n, "f"), []byte("x"), 0o600)
	}
	removed := tools.ReapOrphanDirs(root, func(n string) bool { return n == "running" })
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "running", "f")); err != nil {
		t.Fatal("a running attempt's directory was removed")
	}
}

func TestRunnerDirRefusesLooseMode(t *testing.T) {
	d := filepath.Join(t.TempDir(), "loose")
	os.Mkdir(d, 0o755)
	if _, err := tools.OpenRunnerDir(d, 4, 1<<20); err == nil {
		t.Fatal("a 0755 directory was accepted as runner-only")
	}
}

func TestReadWindowOnWorktreeFile(t *testing.T) {
	f := newFixture(t)
	_, rd, _ := f.spillSetup(t)
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "l%d\n", i)
	}
	write(t, filepath.Join(f.wt, "n.txt"), sb.String())
	out, isErr := rd.Run(context.Background(), readIn("n.txt", 10, 3))
	if isErr || !strings.HasPrefix(out, "l10\nl11\nl12\n") || !strings.Contains(out, "offset 13") {
		t.Fatalf("%v %q", isErr, out)
	}
	out, _ = rd.Run(context.Background(), readIn("n.txt", 99, 50))
	if out != "l99\nl100\n" {
		t.Fatalf("tail window: %q", out)
	}
	if _, isErr := rd.Run(context.Background(), readIn("n.txt", 0, 0)); isErr {
		t.Fatal("plain read broke")
	}
}
