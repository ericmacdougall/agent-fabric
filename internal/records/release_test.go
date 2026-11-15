package records

import (
	"errors"
	"strings"
	"testing"
)

// Spec v16 AC-34: a release_authorized record needs its generation and a
// reason that is one line of printable ASCII, at most 200 bytes, with no
// credential shape (the operator's text never carries a credential into a
// record); the generation is part of the identity, so a second release of
// a newer hold on the same attempt is recorded.
func TestReleaseAuthorizedRecord(t *testing.T) {
	dir := t.TempDir()
	ok := Record{Mission: "m_r", Attempt: "r_r_1", Kind: ReleaseAuthorized, Class: "operator_release", Generation: 7, Reason: "rebound the token"}
	if err := Append(dir, ok); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]string{
		"empty": "", "long": strings.Repeat("a", 201), "newline": "a\nb", "non-ascii": "café",
		"token": "see " + strings.Join([]string{"sk", "ant", "oat01", "AAAAAAAAAAAA"}, "-"), "dsn": "postgres://u:pw@h/db", "entropy": "x Q7mZp2Xv9LkR4tBn8WcY1HsJ6dFg3NaE5uKoTrVi",
	} {
		bad := ok
		bad.Reason = r
		if err := Append(dir, bad); !errors.Is(err, ErrRecord) {
			t.Errorf("%s reason accepted: %v", name, err)
		}
	}
	nogen := ok
	nogen.Generation = 0
	if err := Append(dir, nogen); !errors.Is(err, ErrRecord) {
		t.Error("release_authorized without a generation accepted")
	}
	stray := Record{Mission: "m_r", Attempt: "r_r_1", Kind: MissionHold, Class: "redaction_numeric_hit", Reason: "x"}
	if err := Append(dir, stray); !errors.Is(err, ErrRecord) {
		t.Error("a reason on another kind accepted")
	}
	again := ok
	again.Generation = 8
	if err := Append(dir, again); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, ok); err != nil {
		t.Fatal(err)
	}
	rs, _ := Read(dir, "r_r_1")
	if len(rs) != 2 {
		t.Fatalf("%d records, want generations 7 and 8 once each", len(rs))
	}
}
