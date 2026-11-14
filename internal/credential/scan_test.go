package credential

import (
	"strings"
	"testing"
)

func TestScanTextClasses(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	for _, s := range append([]Secret{b.token}, b.others...) {
		for _, f := range AllForms {
			hit, class := b.ScanText("diagnosis: " + string(Encode(f, s.value)) + " end")
			if !hit || class == "" {
				t.Fatalf("%s/%s not blocked", s.kind, f)
			}
			if strings.Contains(class, string(s.value[:8])) {
				t.Fatal("class carries value bytes")
			}
		}
	}
	for text, want := range map[string]string{
		"see " + PublicLiteral + "x":              ClassSkAnt,
		"POSTGRES://u@h/db":                       ClassPostgres,
		"run " + string(synthSecret(t, 48)) + ".": ClassEntropy,
	} {
		if hit, class := b.ScanText(text); !hit || class != want {
			t.Fatalf("%q: %v %q want %q", text[:8], hit, class, want)
		}
	}
}

// AC-10: ordered 3-, 5- and 7-byte fragments separated by punctuation, JSON
// framing and Markdown each block.
func TestScanTextFragments(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	body := tok[len("sk-ant-oat01-"):] // also without the literal prefix
	for _, size := range []int{3, 5, 7} {
		var parts []string
		for i := 0; i < len(body); i += size {
			e := i + size
			if e > len(body) {
				e = len(body)
			}
			parts = append(parts, string(body[i:e]))
		}
		for name, joined := range map[string]string{
			"punct":    strings.Join(parts, ". "),
			"json":     `["` + strings.Join(parts, `","`) + `"]`,
			"markdown": "**" + strings.Join(parts, "** _") + "_",
		} {
			hit, class := b.ScanText(joined)
			if !hit {
				t.Fatalf("%d-byte %s fragments not blocked", size, name)
			}
			_ = class
		}
	}
}

// AC-10 must-pass: a realistic canary diagnosis quoting worktree paths,
// lib.sh line numbers, the mission id and the attempt id is not blocked.
func TestScanTextDiagnosisPasses(t *testing.T) {
	b := testBound(t, synthToken(t, 108))
	mission := "m-01JD3ZQ7K9XKQ2V8W5T4R3P2N1"
	attempt := "a-01JD3ZQ8B2C4D6F8G0H2J4K6M8"
	path := "/srv/fabric/runs/a-01JD3ZQ8B2C4D6F8G0H2J4K6M8/worktree/lib.sh"
	text := "Diagnosis for " + mission + " (attempt " + attempt + "):\n" +
		"`./test.sh` fails: expected 5, got 4. In " + path + " line 17, add() uses `$(( $1 + $2 - 1 ))`.\n" +
		"lib.sh:17 and lib.sh:23 disagree; test.sh:9 asserts add 2 3 == 5. Fix: drop the `- 1` at lib.sh:17.\n" +
		"Commit 8297e422ad519e23152e7a21119ed9bc5c8991ad is the canary."
	if hit, class := b.ScanTextExcluding(text, []string{path, mission, attempt, "8297e422ad519e23152e7a21119ed9bc5c8991ad"}); hit {
		t.Fatalf("realistic diagnosis blocked: %s", class)
	}
}

// Owner ruling conditions (3) and (4) on the entropy exclusion.
func TestScanTextExclusionNegatives(t *testing.T) {
	tok := synthToken(t, 108)
	b := testBound(t, tok)
	listed := "/srv/fabric/runs/a-1/worktree/lib.sh"
	excl := []string{listed, "m-1", "a-1"}
	// (3a) a token or its base64 inside a path-like string not in the list.
	for _, v := range [][]byte{tok, Encode(FormB64, tok)} {
		fake := "/srv/fabric/runs/a-1/worktree/" + string(v) + "/x.sh"
		if hit, _ := b.ScanTextExcluding("see "+fake, excl); !hit {
			t.Fatal("credential inside an unlisted path-like string not blocked")
		}
	}
	// (3b) a run holding a whole excluded string plus extra high-entropy characters.
	extra := string(synthSecret(t, 40))
	if hit, class := b.ScanTextExcluding("see /srv/fabric/runs/a-1/worktree/"+extra+" ok", []string{"/srv/fabric/runs/a-1/worktree/"}); !hit || class != ClassEntropy {
		t.Fatalf("superset run not blocked: %v %q", hit, class)
	}
	// (4) a file-list entry that itself holds a bound credential form.
	bad := "/srv/x/" + string(Encode(FormHexLower, tok)) + ".sh"
	if hit, class := b.ScanTextExcluding("see "+bad, []string{bad}); !hit || !strings.HasPrefix(class, ClassFormPrefix) {
		t.Fatalf("listed entry with a credential form not blocked: %v %q", hit, class)
	}
}
