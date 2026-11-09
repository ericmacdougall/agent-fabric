package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/envelope.md quotes the vector file's hash and the hand-worked vector,
// so the page cannot drift from the file the tests load.
func TestDocQuotesVectorFile(t *testing.T) {
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "envelope.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), hex.EncodeToString(sum[:])) {
		t.Fatalf("docs/envelope.md does not quote the vector file hash %x", sum)
	}
	v := loadVectors(t).Cases[0]
	if !strings.Contains(string(doc), v.EventID) {
		t.Fatalf("docs/envelope.md does not quote the first vector's event id %s", v.EventID)
	}
}
