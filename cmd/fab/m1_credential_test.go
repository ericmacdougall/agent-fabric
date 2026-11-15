package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/checkout"
	"fabric/internal/credential"
)

// statOverride reports a chosen owner or mode for one path and the real
// metadata elsewhere.
type statOverride struct {
	path string
	uid  *uint32
	mode *os.FileMode
}

func (s statOverride) Lstat(p string) (checkout.Info, error) {
	i, err := checkout.OS{}.Lstat(p)
	if err != nil || p != s.path {
		return i, err
	}
	if s.uid != nil {
		i.UID = *s.uid
	}
	if s.mode != nil {
		i.Mode = os.ModeDir | *s.mode
	}
	return i, nil
}
func (s statOverride) Fstat(f *os.File) (checkout.Info, error) { return checkout.OS{}.Fstat(f) }

func testWho() checkout.Principals {
	return checkout.Principals{OperatorUID: uint32(os.Getuid()), WorkerUID: 999999, WorkerGIDs: []uint32{999998}}
}

// privateRoot is a temp dir whose ancestors pass the worker-writability
// check: under $HOME rather than a sticky world-writable /tmp.
func privateRoot(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp(os.Getenv("HOME"), ".fab-runarea-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkout.CheckAncestors(checkout.OS{}, testWho(), base, "test base"); err != nil {
		t.Skipf("the test base has a worker-writable ancestor here: %v", err)
	}
	return base
}

// Owner ruling A: the run area is a fabd-owned 0700 directory with no
// worker-writable ancestor; anything else refuses start.
func TestPrepareRunArea(t *testing.T) {
	base := privateRoot(t)
	root := filepath.Join(base, "run-area")
	ra, err := prepareRunArea(root, testWho(), checkout.OS{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ra.Root, ra.Streams, ra.Bindings, ra.Records} {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, err, fi.Mode())
		}
	}
	if _, err := prepareRunArea(root, testWho(), checkout.OS{}); err != nil {
		t.Fatalf("an existing good run area is refused: %v", err)
	}
	other := uint32(os.Getuid() + 1)
	open := os.FileMode(0o750)
	worldW := os.FileMode(0o777)
	cases := map[string]checkout.Stater{
		"run area owned by another uid": statOverride{path: root, uid: &other},
		"run area mode 0750":            statOverride{path: root, mode: &open},
		"records dir mode 0750":         statOverride{path: filepath.Join(root, "records"), mode: &open},
		"worker-writable ancestor":      statOverride{path: base, mode: &worldW},
	}
	for name, st := range cases {
		if _, err := prepareRunArea(root, testWho(), st); err == nil {
			t.Errorf("%s: start not refused", name)
		}
	}
	if _, err := prepareRunArea("run-area", testWho(), checkout.OS{}); err == nil {
		t.Error("relative run area not refused")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRunArea(link, testWho(), checkout.OS{}); err == nil {
		t.Error("symlinked run area not refused")
	}
	// The worker profile's extra read roots never include the run area.
	for _, r := range m1ReadRoots(base, filepath.Join(base, "hooks")) {
		if strings.HasPrefix(root, r) {
			t.Fatalf("worker read root %s contains the run area", r)
		}
	}
	if def := m1DefaultRunArea(base); def != root {
		t.Fatalf("default run area %s, want %s", def, root)
	}
}

func TestLoadIDKeyOnceAndChecked(t *testing.T) {
	dir := t.TempDir()
	k1, err := loadIDKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, err := loadIDKey(dir)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("id key not stable across starts")
	}
	p := filepath.Join(dir, runAreaIDKey)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIDKey(dir); err == nil {
		t.Fatal("0644 id key accepted")
	}
	os.Chmod(p, 0o600)
	if err := os.WriteFile(p, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIDKey(dir); err == nil {
		t.Fatal("short id key accepted")
	}
}

// AC-18: the other credentials are read at each call, so a change between
// launch and settle shows as a different version id.
func TestM1OthersReadFresh(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "member.key")
	os.WriteFile(kf, []byte("synthetic-goodqa-key-AAAA\n"), 0o600)
	f := m1OthersFunc(kf, "postgres://runner:synthetic-runner-pw@127.0.0.1:5432/fabric")
	a := f()
	if len(a) != 2 || a[0].Kind != credential.KindGoodQA || string(a[0].Value) != "synthetic-goodqa-key-AAAA" ||
		a[1].Kind != credential.KindDSN || string(a[1].Value) != "synthetic-runner-pw" {
		t.Fatalf("others: %d entries", len(a))
	}
	os.WriteFile(kf, []byte("synthetic-goodqa-key-BBBB\n"), 0o600)
	if b := f(); string(b[0].Value) != "synthetic-goodqa-key-BBBB" {
		t.Fatal("goodqa key not reread")
	}
	if got := m1OthersFunc(kf, "postgres://runner@127.0.0.1/fabric")(); len(got) != 1 {
		t.Fatalf("a DSN without a password adds an entry (%d)", len(got))
	}
}
