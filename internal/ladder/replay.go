package ladder

// Replay and Status are an exact port of the harness ladder,
// escalation.py rung_of and ladder_status (goodqa#15324). Resolve
// (ladder.go) predates that ledger and counts differently: it does not reset on a new
// finding fingerprint and never re-arms a head after a claimed fix. Replay
// is the one to use when the answer must agree with escalation.py.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// PolicyRung is one rung of model_tiers.json.
type PolicyRung struct {
	Writable            bool `json:"writable"`
	NeedsUserPermission bool `json:"needs_user_permission"`
	// ReviewerRung is optional display data (goodqa#15324 S1b): the rung one
	// above, from model_tiers.json. Empty when the policy file omits it.
	ReviewerRung string `json:"reviewer_rung,omitempty"`
}

// Limits are model_tiers.json ladder_limits. MaxTotalReviewUSD keeps the
// number's text; Status compares it the way Python does (pyNumber).
type Limits struct {
	MaxReviewLaunches int         `json:"max_review_launches"`
	MaxRungClimbs     int         `json:"max_rung_climbs"`
	MaxTotalReviewUSD json.Number `json:"max_total_review_usd"`
}

// Policy is the subset of model_tiers.json the ladder reads.
type Policy struct {
	LowestRung      string                `json:"lowest_rung"`
	FailuresPerRung int                   `json:"failures_per_rung"`
	Rungs           map[string]PolicyRung `json:"rungs"`
	Limits          Limits                `json:"ladder_limits"`
}

// LedgerEvent is one escalation.py event. Only Class FAIL and CLAIM affect
// the rung; every other class (PASS, NOT_COUNTED, ...) is ignored, as in
// rung_of. A nil Fingerprint or Head is Python None.
type LedgerEvent struct {
	Class       string  `json:"class"`
	Fingerprint *string `json:"fingerprint"`
	Head        *string `json:"head"`
}

// Computed is rung_of's result.
type Computed struct {
	Rung           string  `json:"rung"`
	FailuresAtRung int     `json:"failures_at_rung"`
	Fingerprint    *string `json:"fingerprint"`
	Climbs         int     `json:"climbs"`
}

// LedgerState is the part of an escalation.py state file Status reads.
// SpendUSD keeps the text of spend_usd (see decimal).
type LedgerState struct {
	Task     string        `json:"task"`
	Events   []LedgerEvent `json:"events"`
	Launches int           `json:"launches"`
	SpendUSD json.Number   `json:"spend_usd"`
}

// LadderStatus is ladder_status's result, minus the permission record,
// which is passed through by callers that need it.
type LadderStatus struct {
	Computed
	BlockedBy           []string `json:"blocked_by"`
	NeedsUserPermission bool     `json:"needs_user_permission"`
	Task                string   `json:"task"`
}

var (
	ErrPolicy = errors.New("ladder: invalid policy")
)

func rungNumber(name string) (int, bool) {
	// model_tiers _RUNG is ^R\d{1,2}$ (S1A-RT2-1).
	if len(name) < 2 || len(name) > 3 || name[0] != 'R' {
		return 0, false
	}
	n, err := strconv.Atoi(name[1:])
	if err != nil || n < 0 || strconv.Itoa(n) != name[1:] {
		return 0, false
	}
	return n, true
}

// order returns rung names sorted by number (model_tiers.rung_order).
func (p Policy) order() ([]string, error) {
	names := make([]string, 0, len(p.Rungs))
	for name := range p.Rungs {
		if _, ok := rungNumber(name); !ok {
			return nil, fmt.Errorf("%w: a rung name is not R plus one or two digits", ErrPolicy)
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, _ := rungNumber(names[i])
		b, _ := rungNumber(names[j])
		return a < b
	})
	return names, nil
}

// nextWritingRung is model_tiers.next_writing_rung: "" at the top. A rung
// that is not a writing rung is an error, as in Python.
func (p Policy) nextWritingRung(rung string) (string, error) {
	all, err := p.order()
	if err != nil {
		return "", err
	}
	var writing []string
	for _, name := range all {
		if p.Rungs[name].Writable {
			writing = append(writing, name)
		}
	}
	for i, name := range writing {
		if name == rung {
			if i+1 < len(writing) {
				return writing[i+1], nil
			}
			return "", nil
		}
	}
	return "", fmt.Errorf("%w: the current rung is not a writing rung", ErrPolicy)
}

// Validate refuses a policy Replay cannot use.
func (p Policy) Validate() error {
	if p.FailuresPerRung <= 0 {
		return fmt.Errorf("%w: failures_per_rung must be positive", ErrPolicy)
	}
	lowest, ok := p.Rungs[p.LowestRung]
	if !ok {
		// Name the field, never its value (S1A-RT2-2).
		return fmt.Errorf("%w: lowest_rung is not a defined rung", ErrPolicy)
	}
	if !lowest.Writable {
		// model_tiers.validate: "the lowest rung must be writable" (S1A-RT2-1).
		return fmt.Errorf("%w: the lowest rung must be writable", ErrPolicy)
	}
	if _, err := p.order(); err != nil {
		return err
	}
	if _, err := pyNumber(string(p.Limits.MaxTotalReviewUSD)); err != nil {
		return fmt.Errorf("%w: max_total_review_usd: %v", ErrPolicy, err)
	}
	return nil
}

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Replay folds FAIL and CLAIM events into the current rung, exactly as
// rung_of: a new fingerprint restarts the count; the same fingerprint at
// the same (non-empty) head with no claimed fix since is one failure, not
// two; reaching failures_per_rung climbs to the next writing rung and
// clears the fingerprint; at the top the loop stops and the rung stays.
func Replay(p Policy, events []LedgerEvent) (Computed, error) {
	if err := p.Validate(); err != nil {
		return Computed{}, err
	}
	c := Computed{Rung: p.LowestRung}
	var current, lastHead *string
	fixedSince := true
	for _, e := range events {
		if e.Class == "CLAIM" {
			fixedSince = true
			continue
		}
		if e.Class != "FAIL" {
			continue
		}
		if !eqPtr(e.Fingerprint, current) {
			current, c.FailuresAtRung = e.Fingerprint, 0
		} else if !fixedSince && e.Head != nil && *e.Head != "" && eqPtr(e.Head, lastHead) {
			continue
		}
		c.FailuresAtRung++
		lastHead, fixedSince = e.Head, false
		if c.FailuresAtRung >= p.FailuresPerRung {
			above, err := p.nextWritingRung(c.Rung)
			if err != nil {
				return Computed{}, err
			}
			if above == "" {
				break
			}
			c.Rung, c.FailuresAtRung, current, c.Climbs = above, 0, nil, c.Climbs+1
		}
	}
	c.Fingerprint = current
	return c, nil
}

// pyNumber gives a JSON number the exact value Python's json module gives
// it: an integral token is a Python int (exact), any other token is a
// Python float (the nearest float64, held exactly as a rational). Python
// then compares int and float by exact value, which big.Rat.Cmp matches;
// so 0.29999999999999999 equals 0.3, and an int limit of 2**53+1 is above
// a float spend of 2**53. Empty text is zero (an absent spend_usd defaults
// to 0). Non-finite values are refused rather than compared.
const maxIntDigits = 4300

func pyNumber(text string) (*big.Rat, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return new(big.Rat), nil
	}
	if !strings.ContainsAny(text, ".eE") {
		// CPython 3.11+ refuses int tokens over sys.int_info.default_max_str_digits
		// (4300) and escalation.py then treats the file as corrupt; refuse
		// rather than answer for it (S1A-RT2-3).
		if len(strings.TrimPrefix(text, "-")) > maxIntDigits {
			return nil, errors.New("integer has too many digits")
		}
		r, ok := new(big.Rat).SetString(text)
		if !ok {
			return nil, errors.New("not a number")
		}
		return r, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errors.New("not a finite number")
	}
	return new(big.Rat).SetFloat64(f), nil
}

// Status is ladder_status: the computed rung plus the limits that block
// further review launches. Comparisons match Python: climbs >, launches
// >=, spend >= (see pyNumber).
func Status(p Policy, st LedgerState) (LadderStatus, error) {
	c, err := Replay(p, st.Events)
	if err != nil {
		return LadderStatus{}, err
	}
	spend, err := pyNumber(string(st.SpendUSD))
	if err != nil {
		return LadderStatus{}, fmt.Errorf("ladder: spend_usd: %w", err)
	}
	limit, err := pyNumber(string(p.Limits.MaxTotalReviewUSD))
	if err != nil {
		return LadderStatus{}, fmt.Errorf("%w: max_total_review_usd: %v", ErrPolicy, err)
	}
	blocked := []string{}
	if c.Climbs > p.Limits.MaxRungClimbs {
		blocked = append(blocked, "max_rung_climbs")
	}
	if st.Launches >= p.Limits.MaxReviewLaunches {
		blocked = append(blocked, "max_review_launches")
	}
	if spend.Cmp(limit) >= 0 {
		blocked = append(blocked, "max_total_review_usd")
	}
	return LadderStatus{
		Computed:            c,
		BlockedBy:           blocked,
		NeedsUserPermission: p.Rungs[c.Rung].NeedsUserPermission,
		Task:                st.Task,
	}, nil
}
