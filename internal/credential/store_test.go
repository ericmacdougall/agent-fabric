package credential

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeWorkerUID, fakeWorkerGID = 999999, 999998

type fixture struct {
	p        Policy
	bindings string
	state    string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "secret")
	must(t, os.Mkdir(dir, 0o750))
	b := filepath.Join(root, "bindings")
	must(t, os.Mkdir(b, 0o700))
	s := filepath.Join(root, "state")
	must(t, os.Mkdir(s, 0o700))
	return fixture{p: Policy{Dir: dir, WorkerUID: fakeWorkerUID, WorkerGIDs: []int{fakeWorkerGID},
		ReadRoots: []string{filepath.Join(root, "runs")}, OwnerUID: -1, GroupGID: -1}, bindings: b, state: s}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reason(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

func nlink(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	must(t, err)
	return statNlink(fi)
}

func TestCaptureRotateLoaderAndGC(t *testing.T) {
	f := newFixture(t)
	v0, a, b := synthToken(t, 108), synthToken(t, 108), synthToken(t, 108)
	n, err := f.p.Capture(v0)
	must(t, err)
	if n != 0 || nlink(t, filepath.Join(f.p.Dir, CurrentName)) != 2 || nlink(t, filepath.Join(f.p.Dir, "claude-oauth.v0")) != 2 {
		t.Fatalf("capture: n=%d", n)
	}
	if fi, _ := os.Lstat(filepath.Join(f.p.Dir, "claude-oauth.v0")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	tok, err := f.p.LoadCurrent()
	must(t, err)
	if tok.Version != 0 || !bytes.Equal(tok.Secret.Value(), v0) {
		t.Fatal("capture not loaded as v0")
	}
	if _, err := f.p.Capture(a); reason(err) != RCaptureExist {
		t.Fatalf("second capture: %v", err)
	}
	// Variant: an unsettled attempt bound to v0 keeps it.
	d, err := f.p.PrepareLaunch(f.bindings, make([]byte, 32), "m1", "att-0", nil)
	must(t, err)
	if d.Binding().TokenVersion != 0 {
		t.Fatal("binding version")
	}
	_, err = f.p.Rotate(a, f.bindings)
	must(t, err)
	_, err = f.p.Rotate(b, f.bindings)
	must(t, err)
	tok, err = f.p.LoadCurrent()
	must(t, err)
	if tok.Version != 2 || !bytes.Equal(tok.Secret.Value(), b) {
		t.Fatalf("rotate: version %d", tok.Version)
	}
	if _, err := os.Lstat(filepath.Join(f.p.Dir, "claude-oauth.v0")); err != nil {
		t.Fatal("v0 bound to an unsettled attempt was deleted")
	}
	if _, err := os.Lstat(filepath.Join(f.p.Dir, "claude-oauth.v1")); err == nil {
		t.Fatal("unbound v1 kept")
	}
	must(t, MarkSettled(f.bindings, "att-0"))
	rep, err := f.p.GC(f.bindings)
	must(t, err)
	if len(rep.Removed) != 1 || rep.Removed[0] != 0 {
		t.Fatalf("gc after settle: %+v", rep)
	}
	// Still refuses a symlinked claude-oauth.
	must(t, os.Remove(filepath.Join(f.p.Dir, CurrentName)))
	must(t, os.Symlink("claude-oauth.v2", filepath.Join(f.p.Dir, CurrentName)))
	if _, err := f.p.LoadCurrent(); reason(err) != RSymlink {
		t.Fatalf("symlinked current: %v", err)
	}
}

func TestSecretDirRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(t *testing.T, f *fixture)
		want   string
	}{
		"symlinked current": {func(t *testing.T, f *fixture) {
			must(t, os.Remove(filepath.Join(f.p.Dir, CurrentName)))
			must(t, os.Symlink("claude-oauth.v0", filepath.Join(f.p.Dir, CurrentName)))
		}, RSymlink},
		"third alias": {func(t *testing.T, f *fixture) {
			must(t, os.Link(filepath.Join(f.p.Dir, "claude-oauth.v0"), filepath.Join(f.p.Dir, "alias")))
		}, RLinkRule},
		"second link not a version": {func(t *testing.T, f *fixture) {
			must(t, os.Rename(filepath.Join(f.p.Dir, "claude-oauth.v0"), filepath.Join(f.p.Dir, "other")))
		}, RLinkRule},
		"one-link current": {func(t *testing.T, f *fixture) {
			must(t, os.Remove(filepath.Join(f.p.Dir, "claude-oauth.v0")))
		}, RLinkRule},
		"version with 3 links": {func(t *testing.T, f *fixture) {
			v := synthToken(t, 64)
			must(t, os.WriteFile(filepath.Join(f.p.Dir, "claude-oauth.v7"), v, 0o640))
			must(t, os.Link(filepath.Join(f.p.Dir, "claude-oauth.v7"), filepath.Join(f.p.Dir, "x1")))
			must(t, os.Link(filepath.Join(f.p.Dir, "claude-oauth.v7"), filepath.Join(f.p.Dir, "x2")))
		}, RLinkRule},
		"symlinked version": {func(t *testing.T, f *fixture) {
			must(t, os.Symlink("/etc/hosts", filepath.Join(f.p.Dir, "claude-oauth.v9")))
		}, RSymlink},
		"world bits": {func(t *testing.T, f *fixture) {
			must(t, os.Chmod(filepath.Join(f.p.Dir, CurrentName), 0o644))
		}, RWorldBits},
		"owned by worker":   {func(t *testing.T, f *fixture) { f.p.WorkerUID = os.Getuid() }, RDir}, // the directory check fires first; checkFile is unit-tested below
		"worker group":      {func(t *testing.T, f *fixture) { f.p.WorkerGIDs = []int{os.Getgid(), fileGID(t, f.p.Dir)} }, RGroupWorker},
		"under a read root": {func(t *testing.T, f *fixture) { f.p.ReadRoots = []string{filepath.Dir(f.p.Dir)} }, RUnderRoot},
		"not regular": {func(t *testing.T, f *fixture) {
			must(t, os.Remove(filepath.Join(f.p.Dir, CurrentName)))
			must(t, os.Mkdir(filepath.Join(f.p.Dir, CurrentName), 0o750))
		}, RNotRegular},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tok := synthToken(t, 108)
			_, err := f.p.Capture(tok)
			must(t, err)
			c.mutate(t, &f)
			_, err = f.p.LoadCurrent()
			if reason(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if strings.Contains(err.Error(), string(tok[:12])) {
				t.Fatal("error carries token bytes")
			}
			if f.p.CheckAdmission() == nil {
				t.Fatal("admitted")
			}
		})
	}
}

func fileGID(t *testing.T, dir string) int {
	fi, err := os.Lstat(filepath.Join(dir, CurrentName))
	must(t, err)
	return statGID(fi)
}

func TestCaptureLength(t *testing.T) {
	for _, n := range []int{31, 513} {
		f := newFixture(t)
		tok := synthToken(t, n)
		_, err := f.p.Capture(tok)
		if reason(err) != RLength || !strings.Contains(err.Error(), "credential_length") ||
			strings.Contains(err.Error(), string(tok[:20])) {
			t.Fatalf("%d: %v", n, err)
		}
		if ents, _ := os.ReadDir(f.p.Dir); len(ents) > 1 {
			t.Fatalf("%d: wrote files", n)
		}
	}
}

func others(t *testing.T) []OtherCredential {
	return []OtherCredential{
		{Kind: KindGoodQA, Name: "goodqa", Value: synthSecret(t, 40)},
		{Kind: KindDSN, Name: "runner", Value: synthSecret(t, 24)},
	}
}

// AC-18 (2): launch with A, rotate to B, "restart" (a fresh Policy and key
// load), GC keeps A while unsettled, settle redacts A, GC removes A after.
func TestBindingSurvivesRotationAndRestart(t *testing.T) {
	f := newFixture(t)
	a, b := synthToken(t, 108), synthToken(t, 108)
	_, err := f.p.Capture(a)
	must(t, err)
	key, err := LoadIDKey(f.state)
	must(t, err)
	oth := others(t)
	d, err := f.p.PrepareLaunch(f.bindings, key, "m1", "att-1", oth)
	must(t, err)
	started := 0
	must(t, Launch(d, func() error { started++; return nil }))
	if started != 1 {
		t.Fatal("launch did not start")
	}
	_, err = f.p.Rotate(b, f.bindings)
	must(t, err)
	// restart
	p2 := f.p
	key2, err := LoadIDKey(f.state)
	must(t, err)
	if !bytes.Equal(key, key2) {
		t.Fatal("id key changed across restart")
	}
	if _, err := os.Lstat(filepath.Join(f.p.Dir, "claude-oauth.v0")); err != nil {
		t.Fatal("A deleted while unsettled")
	}
	bound, err := p2.LoadBound(f.bindings, "att-1", key2, oth)
	must(t, err)
	if !bytes.Equal(bound.Token().Value(), a) {
		t.Fatal("bound token is not A")
	}
	mid := len(a) / 2
	r := bound.Redact(jsonLine(string(a[:mid])), []byte(string(a[mid:])+"\n"))
	noGram(t, "out", a, r.Stdout)
	noGram(t, "err", a, r.Stderr)
	if !r.Exposed() {
		t.Fatal("not counted as exposure")
	}
	must(t, MarkSettled(f.bindings, "att-1"))
	_, err = p2.GC(f.bindings)
	must(t, err)
	if _, err := os.Lstat(filepath.Join(f.p.Dir, "claude-oauth.v0")); err == nil {
		t.Fatal("A kept after settle")
	}
}

// AC-18 (3), NEG-24: a failed fsync or rename starts nothing.
func TestBindingWriteFaultStartsNothing(t *testing.T) {
	for _, which := range []string{"sync", "rename"} {
		f := newFixture(t)
		_, err := f.p.Capture(synthToken(t, 108))
		must(t, err)
		boom := func() error { return errors.New("injected") }
		if which == "sync" {
			f.p.faults = &faults{beforeSync: boom}
		} else {
			f.p.faults = &faults{beforeRename: boom}
		}
		d, err := f.p.PrepareLaunch(f.bindings, make([]byte, 32), "m1", "att-2", nil)
		started := 0
		lerr := Launch(d, func() error { started++; return nil })
		if err == nil || started != 0 || !errors.Is(lerr, ErrNoBinding) {
			t.Fatalf("%s: err=%v started=%d", which, err, started)
		}
		if ents, _ := os.ReadDir(f.bindings); len(ents) != 0 {
			t.Fatalf("%s: left %d files", which, len(ents))
		}
	}
}

func settleClass(err error) string {
	var s *SettleError
	if errors.As(err, &s) {
		return s.Class
	}
	return ""
}

// AC-18 (4), (5), NEG-24, NEG-25.
func TestSettleFailsClosed(t *testing.T) {
	cases := map[string]struct {
		mutate func(t *testing.T, f fixture, oth []OtherCredential) []OtherCredential
		want   string
	}{
		"deleted record": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			must(t, os.Remove(filepath.Join(f.bindings, "att-3.json")))
			return o
		}, ClassMaterialUnavailable},
		"truncated record": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			must(t, os.Truncate(filepath.Join(f.bindings, "att-3.json"), 10))
			return o
		}, ClassMaterialUnavailable},
		"altered record": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			must(t, os.WriteFile(filepath.Join(f.bindings, "att-3.json"), []byte(`{"attempt":"att-3","mission":"m1","token_version":0,"others":[],"extra":1}`), 0o600))
			return o
		}, ClassMaterialUnavailable},
		"version deleted": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			_, err := f.p.Rotate(synthToken(t, 108), f.bindings)
			must(t, err)
			must(t, os.Remove(filepath.Join(f.p.Dir, "claude-oauth.v0")))
			return o
		}, ClassMaterialUnavailable},
		"goodqa key changed": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			o[0].Value = synthSecret(t, 40)
			return o
		}, ClassRotatedMidAttempt},
		"dsn password changed": {func(t *testing.T, f fixture, o []OtherCredential) []OtherCredential {
			o[1].Value = synthSecret(t, 24)
			return o
		}, ClassRotatedMidAttempt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.p.Capture(synthToken(t, 108))
			must(t, err)
			key, _ := LoadIDKey(f.state)
			oth := others(t)
			_, err = f.p.PrepareLaunch(f.bindings, key, "m1", "att-3", oth)
			must(t, err)
			now := c.mutate(t, f, append([]OtherCredential(nil), oth...))
			b, err := f.p.LoadBound(f.bindings, "att-3", key, now)
			if b != nil || settleClass(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
		})
	}
}

// AC-18 (7): the binding record holds no credential value in any form.
func TestBindingHoldsNoCredential(t *testing.T) {
	f := newFixture(t)
	tok := synthToken(t, 108)
	_, err := f.p.Capture(tok)
	must(t, err)
	key, _ := LoadIDKey(f.state)
	oth := others(t)
	_, err = f.p.PrepareLaunch(f.bindings, key, "m1", "att-4", oth)
	must(t, err)
	data, err := os.ReadFile(filepath.Join(f.bindings, "att-4.json"))
	must(t, err)
	fi, _ := os.Lstat(filepath.Join(f.bindings, "att-4.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("binding mode %v", fi.Mode())
	}
	b, _ := NewBound(NewSecret(KindToken, "x", tok), NewSecret(KindGoodQA, "g", oth[0].Value), NewSecret(KindDSN, "d", oth[1].Value))
	if r := b.Redact(nil, data); r.Exposed() {
		t.Fatalf("binding record holds a credential form: %v", SortedLabels(r.Counts))
	}
}

func TestCheckFileOwnerWorker(t *testing.T) {
	p := Policy{WorkerUID: 1234, WorkerGIDs: []int{55}}
	reg := statT{mode: 0o100640, uid: 1234, gid: 7}
	if reason(p.checkFile("claude-oauth", reg)) != ROwnerWorker {
		t.Fatal("worker-owned token accepted")
	}
	reg.uid = 1
	if err := p.checkFile("claude-oauth", reg); err != nil {
		t.Fatal(err)
	}
}
