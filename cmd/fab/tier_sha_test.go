package main

import (
	"os"
	"path/filepath"
	"testing"
)

// goodqa#15172: the tier-table digest is never empty, for any valid file.
// A multi-line or backslash-carrying table used to give an empty digest,
// which the grant refuses, so every fabd launch died at prelaunch.
func TestTierTableSHAMultiline(t *testing.T) {
	tables := map[string]string{
		"one line":   `{"rungs":{"r1":{"effort":"medium","models":{"claude":"m"}}}}`,
		"multi-line": "{\n  \"rungs\": {\"r1\": {\"effort\": \"medium\", \"models\": {\"claude\": \"m\"}}}\n}\n",
		"backslash":  `{"note":"a\\b","rungs":{"r1":{"effort":"medium","models":{"claude":"m"}}}}`,
		"tab":        "{\t\"rungs\":{\"r1\":{\"effort\":\"medium\",\"models\":{\"claude\":\"m\"}}}}",
	}
	seen := map[string]string{}
	for name, body := range tables {
		p := filepath.Join(t.TempDir(), "tiers.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		model, effort, sha, err := resolveRung(p, "r1")
		if err != nil || model != "m" || effort != "medium" {
			t.Fatalf("%s: %q %q %v", name, model, effort, err)
		}
		if len(sha) != 16 {
			t.Fatalf("%s: tier-table sha %q, want 16 hex characters", name, sha)
		}
		if other, dup := seen[sha]; dup {
			t.Fatalf("%s and %s share sha %s", name, other, sha)
		}
		seen[sha] = name
	}
	// goodqa#15178: encoding/json turns invalid bytes into U+FFFD, so two
	// tables differing only there would share a digest. Such a table is
	// refused, in the body or in a model name.
	for name, body := range map[string]string{
		"invalid in note":  "{\"note\":\"a\xffb\",\"rungs\":{\"r1\":{\"effort\":\"medium\",\"models\":{\"claude\":\"m\"}}}}",
		"invalid in model": "{\"rungs\":{\"r1\":{\"effort\":\"medium\",\"models\":{\"claude\":\"m\xfe\"}}}}",
	} {
		p := filepath.Join(t.TempDir(), "tiers.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, sha, err := resolveRung(p, "r1"); err == nil {
			t.Fatalf("%s: accepted with sha %q, want a refusal", name, sha)
		}
	}
	// A table the old code digested correctly keeps its digest:
	// 8d5bc40398291c81 is what 6bd41c8 gave for the one-line table.
	if got, err := domainSHA([]byte(tables["one line"])); err != nil || got != "8d5bc40398291c81" {
		t.Fatalf("one-line digest %q (%v), want the pre-fix 8d5bc40398291c81", got, err)
	}
}
