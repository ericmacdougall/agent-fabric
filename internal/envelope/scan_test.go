package envelope

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every event decode goes through DecodeEvent (or Upcast, which calls it):
// a direct json.Unmarshal into a domain.Event anywhere else would let a stored
// row be read without the v2 reader's checks and without a hold on failure.
var directDecode = []*regexp.Regexp{
	regexp.MustCompile(`var \w+ domain\.Event\b[^\n]*\n(?:[^\n]*\n){0,3}?[^\n]*json\.Unmarshal\(`),
	regexp.MustCompile(`json\.Unmarshal\([^\n]*&\w+\.Event\)`),
	regexp.MustCompile(`json\.Unmarshal\([^\n]*&\[\]domain\.Event\b`),
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found")
	return ""
}

func TestNoDirectEventDecode(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "tools":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(rel, "internal/envelope/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, re := range directDecode {
			if loc := re.FindIndex(b); loc != nil {
				t.Errorf("%s decodes an event directly; use envelope.DecodeEvent or envelope.Upcast:\n%s", rel, b[loc[0]:loc[1]])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("the scan saw %d files; it is vacuous", scanned)
	}
}

// The scan itself must catch what it claims to: a planted direct decode.
func TestDirectDecodePatternsMatch(t *testing.T) {
	bad := []string{
		"var e domain.Event\nif err := json.Unmarshal(payload, &e); err != nil {",
		"if err := json.Unmarshal(payload, &r.Event); err != nil {",
		"json.Unmarshal(b, &[]domain.Event{})",
	}
	for _, s := range bad {
		hit := false
		for _, re := range directDecode {
			if re.MatchString(s) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("pattern set misses %q", s)
		}
	}
}
