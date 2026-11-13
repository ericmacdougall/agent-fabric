package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fabric/internal/tools"
)

// goodqa#15864: what the approver is shown is computed from the file as it
// is, and is exactly what the tool would then write.

func TestEditPreviewShowsPathShaAndDiffAndWritesNothing(t *testing.T) {
	e := newEditEnv(t)
	p := e.put(t, "pkg/a.txt", "one\ntwo\nthree\n")
	before, _ := os.ReadFile(p)
	pv, err := e.ed.Preview(context.Background(), edits("pkg/a.txt", "two", "TWO"))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Path != "pkg/a.txt" || pv.TargetSHA256 != sha(before) || pv.NewSHA256 != sha([]byte("one\nTWO\nthree\n")) || pv.DiffSHA256 == "" ||
		!strings.Contains(pv.Shown, "-two") || !strings.Contains(pv.Shown, "+TWO") || pv.Truncated {
		t.Fatalf("preview: %+v", pv)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("a preview changed the file")
	}
	// the same call previews the same, and Run then writes exactly what was previewed
	pv2, _ := e.ed.Preview(context.Background(), edits("pkg/a.txt", "two", "TWO"))
	if pv2 != pv {
		t.Fatalf("a preview is not deterministic:\n%+v\n%+v", pv, pv2)
	}
	if out, isErr := e.ed.Run(context.Background(), edits("pkg/a.txt", "two", "TWO")); isErr || !strings.Contains(out, "post_sha256: "+pv.NewSHA256) {
		t.Fatalf("run did not write what was previewed: %s", out)
	}
	// a different file state, or a different change, previews differently
	pv3, err := e.ed.Preview(context.Background(), edits("pkg/a.txt", "three", "3"))
	if err != nil || pv3.TargetSHA256 == pv.TargetSHA256 || pv3.DiffSHA256 == pv.DiffSHA256 {
		t.Fatalf("preview after the change: %+v %v", pv3, err)
	}
}

func TestWritePreviewNewFileOverwriteAndNoDirectories(t *testing.T) {
	e := newEditEnv(t)
	pv, err := e.wr.Preview(context.Background(), wr("deep/er/new.txt", "fresh\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Path != "deep/er/new.txt" || pv.TargetSHA256 != "new" || !strings.Contains(pv.Shown, "+fresh") || !strings.Contains(pv.Shown, "--- new content ---\nfresh") {
		t.Fatalf("new file preview: %+v", pv)
	}
	if _, err := os.Stat(filepath.Join(e.wt, "deep")); err == nil {
		t.Fatal("a preview created a directory")
	}
	e.put(t, "o.txt", "old\n")
	pv, err = e.wr.Preview(context.Background(), wr("o.txt", "new\n"))
	if err != nil || pv.TargetSHA256 != sha([]byte("old\n")) || !strings.Contains(pv.Shown, "-old") || !strings.Contains(pv.Shown, "--- new content ---\nnew") {
		t.Fatalf("overwrite preview: %+v %v", pv, err)
	}
}

func TestPreviewRefusesWhatRunRefuses(t *testing.T) {
	e := newEditEnv(t)
	e.put(t, "f.txt", "x\n")
	if err := os.Symlink(e.outside, filepath.Join(e.wt, "link")); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"edit .git":  func() error { _, err := e.ed.Preview(context.Background(), edits(".git/config", "a", "b")); return err },
		"write .git": func() error { _, err := e.wr.Preview(context.Background(), wr(".git/hooks/x", "y")); return err },
		"edit dot-dot": func() error {
			_, err := e.ed.Preview(context.Background(), edits("../outside/x", "a", "b"))
			return err
		},
		"write absolute":    func() error { _, err := e.wr.Preview(context.Background(), wr("/etc/passwd", "y")); return err },
		"write via symlink": func() error { _, err := e.wr.Preview(context.Background(), wr("link/x", "y")); return err },
		"edit missing file": func() error { _, err := e.ed.Preview(context.Background(), edits("nope.txt", "a", "b")); return err },
		"edit no match":     func() error { _, err := e.ed.Preview(context.Background(), edits("f.txt", "absent", "b")); return err },
		"edit changes none": func() error { _, err := e.ed.Preview(context.Background(), edits("f.txt", "x", "x")); return err },
		"write wrong expect": func() error {
			_, err := e.wr.Preview(context.Background(), wr("f.txt", "y", "expect_sha256", sha([]byte("other"))))
			return err
		},
		"edit bad input": func() error {
			_, err := e.ed.Preview(context.Background(), json.RawMessage(`{"path":"f.txt"}`))
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: previewed", name)
		}
	}
}

func TestPreviewCapsTheShownChangeAndKeepsTheFullDigest(t *testing.T) {
	e := newEditEnv(t)
	big := strings.Repeat("a long line of changing content\n", 4000) // 128 KiB
	e.put(t, "big.txt", "head\n")
	pv, err := e.wr.Preview(context.Background(), wr("big.txt", big))
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Truncated || !strings.Contains(pv.Shown, "[TRUNCATED diff:") || !strings.Contains(pv.Shown, "[TRUNCATED new content:") ||
		!strings.Contains(pv.Shown, pv.DiffSHA256) || len(pv.Shown) > 3*tools.MaxPreviewBytes {
		t.Fatalf("not capped with a clear marker (len %d, truncated %v)", len(pv.Shown), pv.Truncated)
	}
	// two changes that agree for the first MaxPreviewBytes but differ later have different digests
	other := big + "and one more line\n"
	pv2, err := e.wr.Preview(context.Background(), wr("big.txt", other))
	if err != nil || pv2.DiffSHA256 == pv.DiffSHA256 || pv2.NewSHA256 == pv.NewSHA256 {
		t.Fatalf("a hidden tail does not change the digests: %+v %v", pv2, err)
	}
	// invalid UTF-8 and a cut inside a rune still yield valid UTF-8 text
	bad := "\xff\xfe" + strings.Repeat("é", tools.MaxPreviewBytes)
	pv3, err := e.wr.Preview(context.Background(), wr("big.txt", bad))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct{ S string }
	if b, err := json.Marshal(struct{ S string }{pv3.Shown}); err != nil || json.Unmarshal(b, &probe) != nil || probe.S != strings.ToValidUTF8(pv3.Shown, "�") {
		t.Fatalf("the shown text is not valid UTF-8")
	}
}
