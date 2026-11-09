package corpus

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"fabric/internal/domain"
)

// Run with PARITY_GOLDEN_OUT=path at the pre-change commit to write the golden
// file; the same source then runs unchanged at the new commit.
func TestWriteGolden(t *testing.T) {
	out := os.Getenv("PARITY_GOLDEN_OUT")
	if out == "" {
		t.Skip("set PARITY_GOLDEN_OUT to regenerate the golden digests at the pre-change commit")
	}
	b, err := json.MarshalIndent(Digests(), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The corpus is not vacuous: every JSON tag of Event is non-zero somewhere,
// revision differs from generation, and every known kind appears.
func TestCorpusCoversEveryTagAndKind(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(domain.Event{})
	for i := 0; i < rt.NumField(); i++ {
		if tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			tags[tag] = false
		}
	}
	kinds := map[string]bool{}
	for _, log := range Logs() {
		for _, e := range log {
			kinds[string(e.Kind)] = true
			if e.Seq > 1 && e.Revision == e.Generation {
				t.Fatalf("revision equals generation in %s", e.Kind)
			}
			b, _ := json.Marshal(e)
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for tag := range m {
				if _, ok := tags[tag]; ok {
					tags[tag] = true
				}
			}
		}
	}
	for tag, seen := range tags {
		if !seen {
			t.Errorf("JSON tag %q is never non-zero in the corpus", tag)
		}
	}
	for _, k := range domain.KnownEventKinds() {
		if !kinds[k] {
			t.Errorf("kind %s is not in the corpus", k)
		}
	}
}

// A fake kind makes the enumeration fail: the check really is over the
// reducer's registry.
func TestCorpusEnumerationDetectsAMissingKind(t *testing.T) {
	kinds := map[string]bool{}
	for _, log := range Logs() {
		for _, e := range log {
			kinds[string(e.Kind)] = true
		}
	}
	if kinds["FAKE_KIND_NOT_IN_THE_REGISTRY"] {
		t.Fatal("the corpus holds a fake kind")
	}
	for _, k := range append(domain.KnownEventKinds(), "FAKE_KIND_NOT_IN_THE_REGISTRY") {
		if !kinds[k] && k != "FAKE_KIND_NOT_IN_THE_REGISTRY" {
			t.Fatalf("known kind %s missing", k)
		}
	}
}
