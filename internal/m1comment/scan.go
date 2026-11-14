package m1comment

import (
	"math"
	"regexp"
	"strings"
)

// BoundCredentials is the credential set bound to the attempt (AC-18), as W3
// provides it (credential.Bound). ScanText reports whether text holds any
// representation-policy form of a bound credential (the token version the
// attempt was given, the GoodQA key, each DSN password), including the AC-10
// fragment check on the bound token, and returns a class or label only,
// never a credential byte.
//
// ScanTextExcluding is the AC-10 entry point of W3's matcher: it also runs
// the literal and entropy rules, with the exclusions applying only to the
// entropy rule. Scan below repeats the literal and entropy rules itself, so
// the composition holds even if an implementation omits them.
type BoundCredentials interface {
	ScanText(text string) (hit bool, class string)
	ScanTextExcluding(text string, exclusions []string) (hit bool, class string)
}

// Allow lists the only strings exempt from the entropy rule (AC-10): runs
// that equal a path in the extract's file list, the mission id or the
// attempt id.
type Allow struct {
	Paths   []string
	Mission string
	Attempt string
}

// exempt: the run equals, or is a piece of, an allowed string. The run
// grammar excludes '.', so "lib.sh" or "/wt/r_x/lib.sh" reaches the scan as
// pieces; a piece of a fabd-supplied path or id cannot carry model-chosen
// bytes. Every allowed string comes from fabd, never from the model.
func (a Allow) exempt(run string) bool {
	for _, s := range append([]string{a.Mission, a.Attempt}, a.Paths...) {
		if s != "" && strings.Contains(s, run) {
			return true
		}
	}
	return false
}

// list is the exclusion list for ScanTextExcluding: only fabd-supplied
// strings (the extract listing and the ids), never model output.
func (a Allow) list() []string {
	var out []string
	for _, s := range append([]string{a.Mission, a.Attempt}, a.Paths...) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Scan classes. Each is a fixed code for a delivery_blocked record.
const (
	ClassCredential  = "scan_credential"
	ClassSkAnt       = "scan_sk_ant"
	ClassPostgresURL = "scan_postgres_url"
	ClassEntropy     = "scan_high_entropy"
	ClassNoCreds     = "scan_no_bound_credentials"
)

var (
	skAntRE   = regexp.MustCompile(`(?i)sk-ant-`)
	pgRE      = regexp.MustCompile(`(?i)postgres(ql)?://`)
	entropyRE = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
)

// MinEntropy is the AC-10 threshold in bits per character.
const MinEntropy = 4.5

// Scan checks the model text (never the fixed header) before Submit. It
// returns blocked=true and a class on the first rule that matches. A nil
// credential set blocks: the scan cannot be skipped by a missing binding.
func Scan(modelText string, creds BoundCredentials, allow Allow) (blocked bool, class string) {
	if creds == nil {
		return true, ClassNoCreds
	}
	if hit, c := creds.ScanTextExcluding(modelText, allow.list()); hit {
		if c == "" || !classRE.MatchString(c) {
			c = ClassCredential
		}
		return true, ClassCredential + ":" + c
	}
	if skAntRE.MatchString(modelText) {
		return true, ClassSkAnt
	}
	if pgRE.MatchString(modelText) {
		return true, ClassPostgresURL
	}
	for _, run := range entropyRE.FindAllString(modelText, -1) {
		if allow.exempt(run) {
			continue
		}
		if Entropy(run) >= MinEntropy {
			return true, ClassEntropy
		}
	}
	return false, ""
}

var classRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,60}$`)

// Entropy is the Shannon entropy of s in bits per character.
func Entropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// StripAlnum removes every byte outside [A-Za-z0-9] (the AC-10 fragment
// check normalisation). Exported so the bound-credential implementation and
// its tests share one definition.
func StripAlnum(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// FragmentHit reports whether any FragmentLen consecutive characters of the
// stripped text occur inside any stripped form (AC-10 fragment check).
func FragmentHit(text string, forms []string) bool {
	st := StripAlnum(text)
	if len(st) < FragmentLen {
		return false
	}
	grams := map[string]struct{}{}
	for _, f := range forms {
		sf := StripAlnum(f)
		for i := 0; i+FragmentLen <= len(sf); i++ {
			grams[sf[i:i+FragmentLen]] = struct{}{}
		}
	}
	for i := 0; i+FragmentLen <= len(st); i++ {
		if _, ok := grams[st[i:i+FragmentLen]]; ok {
			return true
		}
	}
	return false
}

// FragmentLen is the AC-10 fragment window.
const FragmentLen = 8
