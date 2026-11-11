package control

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"fabric/internal/control/ingest"
)

// goodqa#15661 F3: the ingest command's allowed list equals the database seed.
func TestIngestAllowedSourcesMatchSchemaSeed(t *testing.T) {
	b, err := schemaFS.ReadFile("sql/001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`INSERT INTO control\.allowed_sources \(source\) VALUES ([^;]+);`).FindSubmatch(b)
	if m == nil {
		t.Fatal("seed statement not found")
	}
	var seed []string
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1) {
		seed = append(seed, string(q[1]))
	}
	got := append([]string(nil), ingest.AllowedSources...)
	sort.Strings(seed)
	sort.Strings(got)
	if strings.Join(seed, ",") != strings.Join(got, ",") {
		t.Fatalf("ingest.AllowedSources %v differs from the schema seed %v", got, seed)
	}
}
