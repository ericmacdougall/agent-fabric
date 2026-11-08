package ladder

// Regression tests for the S1a delta red-team findings S1A-RT2-1..7
// (goodqa#15324). Each case failed against 9abc522.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rt2Rungs = `"rungs":{"R0":{"writable":true,"needs_user_permission":false},"R1":{"writable":true,"needs_user_permission":false}}`
const rt2Limits = `"ladder_limits":{"max_review_launches":8,"max_rung_climbs":3,"max_total_review_usd":60}`

// RT2-1: rung names are R plus one or two digits (model_tiers _RUNG), and
// the lowest rung must be writable, as model_tiers.validate requires.
func TestRT2PolicyMatchesModelTiersValidate(t *testing.T) {
	for name, body := range map[string]string{
		"three-digit rung":    `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false},"R100":{"writable":true,"needs_user_permission":false}},` + rt2Limits + `}`,
		"lowest not writable": `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":false,"needs_user_permission":false},"R1":{"writable":true,"needs_user_permission":false}},` + rt2Limits + `}`,
	} {
		// Either refusal is exit 3; the decoder catches bad names first.
		if _, err := DecodePolicy("p.json", []byte(body)); ExitCode(err) != 3 {
			t.Errorf("%s: err = %v, want a refusal (ErrInput or ErrPolicy)", name, err)
		}
	}
	// Two-digit rungs stay valid.
	ok := `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false},"R10":{"writable":true,"needs_user_permission":false}},` + rt2Limits + `}`
	if _, err := DecodePolicy("p.json", []byte(ok)); err != nil {
		t.Errorf("R10 policy refused: %v", err)
	}
}

// RT2-2: no policy error repeats a value from the file.
func TestRT2PolicyErrorsNeverEchoValues(t *testing.T) {
	for name, body := range map[string]string{
		"undefined lowest": `{"lowest_rung":"sk-live-SECRETVALUE","failures_per_rung":2,` + rt2Rungs + `,` + rt2Limits + `}`,
		"bad rung name":    `{"lowest_rung":"R0","failures_per_rung":2,"rungs":{"R0":{"writable":true,"needs_user_permission":false},"sk-live-SECRETVALUE":{"writable":true,"needs_user_permission":false}},` + rt2Limits + `}`,
	} {
		_, err := DecodePolicy("p.json", []byte(body))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err %v accepts or echoes the value", name, err)
		}
	}
}

// RT2-3: an integer token longer than CPython's default int_max_str_digits
// (4300) is refused, since Python 3.11+ treats that file as corrupt.
func TestRT2RefusesHugeIntegers(t *testing.T) {
	huge := strings.Repeat("9", 4301)
	body := `{"schema":1,"task":"t","events":[],"spend_usd":` + huge + `}`
	if _, err := DecodeState("t.json", []byte(body)); !errors.Is(err, ErrInput) {
		t.Errorf("4301-digit spend: err = %v, want ErrInput", err)
	}
	ok := `{"schema":1,"task":"t","events":[],"spend_usd":` + strings.Repeat("9", 4300) + `}`
	if _, err := DecodeState("t.json", []byte(ok)); err != nil {
		t.Errorf("4300-digit spend refused: %v", err)
	}
}

// RT2-4: refused input maps to exit 3 and other errors to 1, without a
// database (cmd/fab tests need FABRIC_TEST_DSN).
func TestRT2ExitCodes(t *testing.T) {
	if got := ExitCode(refuse("t.json", "task", "is missing")); got != 3 {
		t.Errorf("ErrInput exit = %d, want 3", got)
	}
	if got := ExitCode(errors.Join(ErrPolicy)); got != 3 {
		t.Errorf("ErrPolicy exit = %d, want 3", got)
	}
	if got := ExitCode(errors.New("disk on fire")); got != 1 {
		t.Errorf("other exit = %d, want 1", got)
	}
	if got := ExitCode(nil); got != 0 {
		t.Errorf("nil exit = %d, want 0", got)
	}
}

// RT2-6: --json escapes every non-ASCII and control rune, so a hostile
// fingerprint cannot drive the terminal, but the decoded value is unchanged.
func TestRT2JSONEscapesNonPrintable(t *testing.T) {
	fp := "ab‮\u009b31m\x1b[0m"
	out, err := Format(LadderStatus{Computed: Computed{Rung: "R0", Fingerprint: &fp}, BlockedBy: []string{}, Task: "t"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r > 0x7e || (r < 0x20 && r != '\n') {
			t.Fatalf("raw rune %U in JSON output", r)
		}
	}
	var back LadderStatus
	if err := json.Unmarshal([]byte(out), &back); err != nil || back.Fingerprint == nil || *back.Fingerprint != fp {
		t.Errorf("round trip changed the fingerprint: %v", err)
	}
}

// RT2-5: the vectors name the escalation.py and model_tiers.py they came
// from. With LADDER_REFERENCE_HOOKS set to a hooks directory, a drifted
// reference fails here instead of passing on stale vectors; without it the
// test only checks that both hashes are recorded.
func TestRT2ReferenceHashesPinned(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var pf parityFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("LADDER_REFERENCE_HOOKS")
	for _, name := range []string{"escalation.py", "model_tiers.py"} {
		want := pf.Reference[name]
		if len(want) != 64 {
			t.Fatalf("parity.json has no sha256 for %s", name)
		}
		if dir == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s drifted: sha256 %s, vectors made from %s; regenerate with tools/ladder-parity/gen.py", name, got[:12], want[:12])
		}
	}
}

// RT2-7: escalation.py only reads <task>.json, so a state file must end in .json.
func TestRT2StateNeedsJSONSuffix(t *testing.T) {
	if _, err := DecodeState("/x/t1", []byte(`{"schema":1,"task":"t1","events":[]}`)); !errors.Is(err, ErrInput) {
		t.Errorf("no suffix: err = %v, want ErrInput", err)
	}
}
