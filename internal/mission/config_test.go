package mission

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fabric/internal/checkout"
	"fabric/internal/domain"
)

const (
	canarySHA = "8297e422ad519e23152e7a21119ed9bc5c8991ad"
	issueUUID = "0b6f7c1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f"
	workerUID = 4242001
	workerGID = 4242002
	thirdUID  = 4242003
)

type fakeStat struct {
	over map[string]func(checkout.Info) checkout.Info
}

func (f fakeStat) apply(p string, i checkout.Info) checkout.Info {
	if fn, ok := f.over[p]; ok {
		return fn(i)
	}
	return i
}

func (f fakeStat) Lstat(p string) (checkout.Info, error) {
	i, err := checkout.OS{}.Lstat(p)
	if err != nil {
		return i, err
	}
	return f.apply(p, i), nil
}

func (f fakeStat) Fstat(file *os.File) (checkout.Info, error) {
	i, err := checkout.OS{}.Fstat(file)
	if err != nil {
		return i, err
	}
	return f.apply(file.Name(), i), nil
}

type fixture struct {
	root, dir, file, brief string
	gid                    uint32
}

func validDoc(brief string) map[string]any {
	return map[string]any{
		"version":  1,
		"issue_id": issueUUID,
		"brief":    brief,
		"repo":     "/srv/fabric/mirrors/canary.git",
		"sha":      canarySHA,
		"caps":     map[string]any{"max_turns": 40, "wall_minutes": 45, "max_budget_usd": 2},
		"tools":    []string{"Read", "Grep", "Glob", "Bash"},
		"allow":    []string{"Bash(./test.sh)"},
	}
}

// newFixture writes a config under a real temp root. The file's real gid
// stands in for fab-secret; the worker is a fake uid outside that group.
func newFixture(t *testing.T, doc any) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{root: root, dir: filepath.Join(root, "m_canary")}
	os.Mkdir(f.dir, 0o750)
	f.brief = filepath.Join(root, "brief.md")
	os.WriteFile(f.brief, []byte("diagnose the failing test\n"), 0o644)
	if doc == nil {
		doc = validDoc(f.brief)
	}
	f.write(t, doc)
	return f
}

func (f *fixture) write(t *testing.T, doc any) {
	t.Helper()
	var b []byte
	switch d := doc.(type) {
	case string:
		b = []byte(d)
	case []byte:
		b = d
	default:
		b, _ = json.Marshal(d)
	}
	f.file = filepath.Join(f.dir, FileName)
	os.Remove(f.file)
	if err := os.WriteFile(f.file, b, 0o640); err != nil {
		t.Fatal(err)
	}
	os.Chmod(f.file, 0o640)
	fi, _ := os.Stat(f.file)
	f.gid = fi.Sys().(*syscall.Stat_t).Gid
}

func (f fixture) loader(st checkout.Stater) Loader {
	if st == nil {
		st = checkout.OS{}
	}
	return Loader{Root: f.root, Stat: st, FabSecretGID: f.gid,
		Who: checkout.Principals{OperatorUID: uint32(os.Getuid()), WorkerUID: workerUID, WorkerGIDs: []uint32{workerGID}}}
}

// AC-22: the canary config loads, with every field reachable.
func TestLoadCanaryConfig(t *testing.T) {
	f := newFixture(t, nil)
	c, err := f.loader(nil).Load("m_canary")
	if err != nil {
		t.Fatal(err)
	}
	if c.SHA() != canarySHA || c.IssueID() != issueUUID || c.Brief() != f.brief || c.Repo() != "/srv/fabric/mirrors/canary.git" || c.Mission() != "m_canary" {
		t.Fatalf("loaded %+v", c)
	}
	if got := c.Caps(); got != (Caps{MaxTurns: 40, Wall: 45 * time.Minute, MaxBudgetUSD: 2}) {
		t.Fatalf("caps %+v", got)
	}
	if strings.Join(c.Tools(), ",") != "Read,Grep,Glob,Bash" {
		t.Fatalf("tools %v", c.Tools())
	}
	c.Tools()[0] = "Edit"
	if c.Tools()[0] != "Read" {
		t.Fatal("Tools exposes the pinned slice")
	}
}

// AC-22: the file is refused unless it is a regular file, mode 0640, owned
// by the operator, group fab-secret, reached through directories the worker
// cannot change.
func TestLoadRefusesUnsafeFile(t *testing.T) {
	type over = map[string]func(checkout.Info) checkout.Info
	for name, c := range map[string]func(f fixture) (Loader, func()){
		"mode 0644": func(f fixture) (Loader, func()) { return f.loader(nil), func() { os.Chmod(f.file, 0o644) } },
		"mode 0600": func(f fixture) (Loader, func()) { return f.loader(nil), func() { os.Chmod(f.file, 0o600) } },
		"mode 0660": func(f fixture) (Loader, func()) { return f.loader(nil), func() { os.Chmod(f.file, 0o660) } },
		"mode 0440": func(f fixture) (Loader, func()) { return f.loader(nil), func() { os.Chmod(f.file, 0o440) } },
		"owned by the worker": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.file: func(i checkout.Info) checkout.Info { i.UID = workerUID; return i }}}), nil
		},
		"owned by root, not the operator": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.file: func(i checkout.Info) checkout.Info { i.UID = 0; return i }}}), nil
		},
		"group not fab-secret": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.file: func(i checkout.Info) checkout.Info { i.GID = workerGID; return i }}}), nil
		},
		"worker in fab-secret": func(f fixture) (Loader, func()) {
			l := f.loader(nil)
			l.Who.WorkerGIDs = append(l.Who.WorkerGIDs, f.gid)
			return l, nil
		},
		"a symlink": func(f fixture) (Loader, func()) {
			return f.loader(nil), func() {
				real := filepath.Join(f.root, "real.json")
				os.Rename(f.file, real)
				os.Symlink(real, f.file)
			}
		},
		"a directory": func(f fixture) (Loader, func()) {
			return f.loader(nil), func() { os.Remove(f.file); os.Mkdir(f.file, 0o750) }
		},
		"mission dir owned by the worker": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.dir: func(i checkout.Info) checkout.Info { i.UID = workerUID; return i }}}), nil
		},
		"mission dir writable by a worker group": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.dir: func(i checkout.Info) checkout.Info { i.GID = workerGID; i.Mode = os.ModeDir | 0o770; return i }}}), nil
		},
		"root ancestor owned by a third user": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{f.root: func(i checkout.Info) checkout.Info { i.UID = thirdUID; return i }}}), nil
		},
		"ancestor writable by others": func(f fixture) (Loader, func()) {
			return f.loader(fakeStat{over{filepath.Dir(f.root): func(i checkout.Info) checkout.Info { i.Mode = os.ModeDir | 0o777; return i }}}), nil
		},
		"worker equals operator": func(f fixture) (Loader, func()) {
			l := f.loader(nil)
			l.Who.WorkerUID = l.Who.OperatorUID
			return l, nil
		},
	} {
		f := newFixture(t, nil)
		l, mutate := c(f)
		if mutate != nil {
			mutate()
		}
		if _, err := l.Load("m_canary"); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
	// Bad mission ids never reach the filesystem.
	f := newFixture(t, nil)
	for _, m := range []domain.MissionID{"", "../m_canary", "m_canary/../x", "."} {
		if _, err := f.loader(nil).Load(m); !errors.Is(err, ErrConfig) {
			t.Errorf("mission id %q: %v", m, err)
		}
	}
}

// AC-22: the brief is read by fab-worker, so it must be readable by it and
// not changeable by it.
func TestLoadChecksBrief(t *testing.T) {
	type over = map[string]func(checkout.Info) checkout.Info
	f := newFixture(t, nil)
	for name, st := range map[string]checkout.Stater{
		"brief owned by the worker":    fakeStat{over{f.brief: func(i checkout.Info) checkout.Info { i.UID = workerUID; return i }}},
		"brief writable by others":     fakeStat{over{f.brief: func(i checkout.Info) checkout.Info { i.Mode = 0o646; return i }}},
		"brief unreadable by worker":   fakeStat{over{f.brief: func(i checkout.Info) checkout.Info { i.Mode = 0o640; return i }}},
		"brief dir owned by third uid": fakeStat{over{f.root: func(i checkout.Info) checkout.Info { i.UID = thirdUID; return i }}},
	} {
		if _, err := f.loader(st).Load("m_canary"); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Readable through the worker's group is enough.
	st := fakeStat{over{f.brief: func(i checkout.Info) checkout.Info { i.GID = workerGID; i.Mode = 0o640; return i }}}
	if _, err := f.loader(st).Load("m_canary"); err != nil {
		t.Errorf("group-readable brief: %v", err)
	}
	os.Remove(f.brief)
	os.Symlink(filepath.Join(f.root, "elsewhere"), f.brief)
	if _, err := f.loader(nil).Load("m_canary"); !errors.Is(err, ErrConfig) {
		t.Errorf("symlinked brief: %v", err)
	}
}

// AC-22, AC-24: the document is parsed strictly and cannot widen the
// pinned tool surface.
func TestParseRefusals(t *testing.T) {
	mod := func(fn func(d map[string]any)) []byte {
		d := validDoc("/srv/fabric/missions/m_canary/brief.md")
		fn(d)
		b, _ := json.Marshal(d)
		return b
	}
	good := string(mod(func(map[string]any) {}))
	cases := map[string][]byte{
		"not json":          []byte("{"),
		"array":             []byte("[]"),
		"trailing data":     []byte(good + "{}"),
		"invalid utf8":      []byte(strings.Replace(good, "canary.git", "canary\xff.git", 1)),
		"unknown key":       mod(func(d map[string]any) { d["model"] = "x" }),
		"case variant key":  []byte(strings.Replace(good, `"sha"`, `"SHA"`, 1)),
		"duplicate key":     []byte(strings.Replace(good, `"sha":`, `"sha":"`+strings.Repeat("0", 40)+`","sha":`, 1)),
		"version 2":         mod(func(d map[string]any) { d["version"] = 2 }),
		"short sha":         mod(func(d map[string]any) { d["sha"] = canarySHA[:12] }),
		"39 hex":            mod(func(d map[string]any) { d["sha"] = canarySHA[:39] }),
		"upper sha":         mod(func(d map[string]any) { d["sha"] = strings.ToUpper(canarySHA) }),
		"ref name":          mod(func(d map[string]any) { d["sha"] = "main" }),
		"bad uuid":          mod(func(d map[string]any) { d["issue_id"] = "goodqa#15966" }),
		"upper uuid":        mod(func(d map[string]any) { d["issue_id"] = strings.ToUpper(issueUUID) }),
		"relative brief":    mod(func(d map[string]any) { d["brief"] = "brief.md" }),
		"unclean repo":      mod(func(d map[string]any) { d["repo"] = "/srv/fabric/../etc" }),
		"root repo":         mod(func(d map[string]any) { d["repo"] = "/" }),
		"tools widened":     mod(func(d map[string]any) { d["tools"] = []string{"Read", "Grep", "Glob", "Bash", "Edit"} }),
		"tools reordered":   mod(func(d map[string]any) { d["tools"] = []string{"Bash", "Read", "Grep", "Glob"} }),
		"tools empty":       mod(func(d map[string]any) { d["tools"] = []string{} }),
		"allow any bash":    mod(func(d map[string]any) { d["allow"] = []string{"Bash"} }),
		"allow bash prefix": mod(func(d map[string]any) { d["allow"] = []string{"Bash(./test.sh *)"} }),
		"allow extra rule":  mod(func(d map[string]any) { d["allow"] = []string{"Bash(./test.sh)", "Read(//**)"} }),
		"caps extra key":    mod(func(d map[string]any) { d["caps"].(map[string]any)["effort"] = "max" }),
		"turns fractional":  mod(func(d map[string]any) { d["caps"].(map[string]any)["max_turns"] = 39.5 }),
		"turns string":      mod(func(d map[string]any) { d["caps"].(map[string]any)["max_turns"] = "40" }),
		"turns huge":        mod(func(d map[string]any) { d["caps"].(map[string]any)["max_turns"] = 1e12 }),
		"wall huge":         mod(func(d map[string]any) { d["caps"].(map[string]any)["wall_minutes"] = 2147483647 }),
		"budget below cent": mod(func(d map[string]any) { d["caps"].(map[string]any)["max_budget_usd"] = 0.001 }),
		"missing caps":      mod(func(d map[string]any) { delete(d, "caps") }),
		"missing issue_id":  mod(func(d map[string]any) { delete(d, "issue_id") }),
		"null sha":          mod(func(d map[string]any) { d["sha"] = nil }),
	}
	for _, k := range []string{"version", "issue_id", "brief", "repo", "sha", "caps", "tools", "allow"} {
		k := k
		cases["missing "+k] = mod(func(d map[string]any) { delete(d, k) })
	}
	for name, b := range cases {
		if _, err := Parse(b); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// AC-25 / NEG-19: each cap at its ceiling passes; one step over any
// ceiling, or a non-positive cap, refuses. The message names the field and
// the ceiling, never the value from the file.
func TestCapCeilings(t *testing.T) {
	set := func(k string, v any) []byte {
		d := validDoc("/srv/fabric/missions/m_canary/brief.md")
		d["caps"].(map[string]any)[k] = v
		b, _ := json.Marshal(d)
		return b
	}
	for _, c := range []struct {
		key string
		val any
		ok  bool
	}{
		{"max_turns", 40, true}, {"max_turns", 1, true}, {"max_turns", 41, false}, {"max_turns", 0, false}, {"max_turns", -1, false}, {"max_turns", 1000, false},
		{"wall_minutes", 45, true}, {"wall_minutes", 46, false}, {"wall_minutes", 0, false}, {"wall_minutes", -45, false},
		{"max_budget_usd", 2, true}, {"max_budget_usd", 0.5, true}, {"max_budget_usd", 2.01, false}, {"max_budget_usd", 2.0000001, false}, {"max_budget_usd", 0, false}, {"max_budget_usd", -1, false},
	} {
		_, err := Parse(set(c.key, c.val))
		if c.ok != (err == nil) {
			t.Errorf("%s=%v: err %v, want ok=%v", c.key, c.val, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "caps."+c.key) {
			t.Errorf("%s=%v: message %q does not name the field", c.key, c.val, err)
		}
	}
	if err := CheckCaps(Caps{MaxTurns: 40, Wall: 45*time.Minute + time.Second, MaxBudgetUSD: 2}); !errors.Is(err, ErrConfig) {
		t.Errorf("wall 45m1s: %v", err)
	}
}

// R2: a refusal never echoes config values (the sha, the issue, the paths).
func TestRefusalsDoNotEchoValues(t *testing.T) {
	secretish := "/srv/fabric/missions/PRIVATE-PATH-MARKER/brief.md"
	for name, fn := range map[string]func(d map[string]any){
		"over cap":  func(d map[string]any) { d["caps"].(map[string]any)["max_turns"] = 99 },
		"bad sha":   func(d map[string]any) { d["sha"] = "PRIVATE-SHA-MARKER" },
		"bad uuid":  func(d map[string]any) { d["issue_id"] = "PRIVATE-ISSUE-MARKER" },
		"bad tools": func(d map[string]any) { d["tools"] = []string{"PRIVATE-TOOL-MARKER"} },
		"bad allow": func(d map[string]any) { d["allow"] = []string{"PRIVATE-RULE-MARKER"} },
		"bad key":   func(d map[string]any) { d["PRIVATE-KEY-MARKER"] = 1 },
		"rel repo":  func(d map[string]any) { d["repo"] = "PRIVATE-REPO-MARKER" },
	} {
		d := validDoc(secretish)
		fn(d)
		b, _ := json.Marshal(d)
		_, err := Parse(b)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "99") {
			t.Errorf("%s: refusal echoes a value: %q", name, err)
		}
	}
}
