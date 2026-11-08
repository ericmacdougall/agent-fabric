package ladder

// Replay and Status are an exact port of the harness ladder,
// escalation.py rung_of and ladder_status (goodqa#15324). Resolve, above,
// predates that ledger and counts differently: it does not reset on a new
// finding fingerprint and never re-arms a head after a claimed fix. Replay
// is the one to use when the answer must agree with escalation.py.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PolicyRung is one rung of model_tiers.json.
type PolicyRung struct {
	Writable            bool `json:"writable"`
	NeedsUserPermission bool `json:"needs_user_permission"`
}

// Limits are model_tiers.json ladder_limits. MaxTotalReviewUSD is kept as
// the exact decimal text from the file, so the cap comparison is exact.
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
// SpendUSD is the exact decimal text of spend_usd.
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
	if len(name) < 2 || name[0] != 'R' {
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
			return nil, fmt.Errorf("%w: rung name %q", ErrPolicy, name)
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
	return "", fmt.Errorf("%w: rung %q is not a writing rung", ErrPolicy, rung)
}

// Validate refuses a policy Replay cannot use.
func (p Policy) Validate() error {
	if p.FailuresPerRung <= 0 {
		return fmt.Errorf("%w: failures_per_rung must be positive", ErrPolicy)
	}
	if _, ok := p.Rungs[p.LowestRung]; !ok {
		return fmt.Errorf("%w: lowest_rung %q is not defined", ErrPolicy, p.LowestRung)
	}
	if _, err := p.order(); err != nil {
		return err
	}
	if _, err := decimal(string(p.Limits.MaxTotalReviewUSD)); err != nil {
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

// decimal parses a JSON number the way Python's json module does: to the
// nearest float64. escalation.py stores and compares spend as a Python
// float, so parity needs the same rounding; an exact decimal would disagree
// on inputs such as 0.29999999999999999, which Python reads as 0.3. Empty
// text is zero (an absent spend_usd defaults to 0). Non-finite values are
// refused rather than compared.
func decimal(text string) (float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("not a finite number: %q", text)
	}
	return f, nil
}

// Status is ladder_status: the computed rung plus the limits that block
// further review launches. Comparisons match Python: climbs >, launches
// >=, spend >= (as float64, like Python).
func Status(p Policy, st LedgerState) (LadderStatus, error) {
	c, err := Replay(p, st.Events)
	if err != nil {
		return LadderStatus{}, err
	}
	spend, err := decimal(string(st.SpendUSD))
	if err != nil {
		return LadderStatus{}, fmt.Errorf("ladder: spend_usd: %w", err)
	}
	limit, err := decimal(string(p.Limits.MaxTotalReviewUSD))
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
	if spend >= limit {
		blocked = append(blocked, "max_total_review_usd")
	}
	return LadderStatus{
		Computed:            c,
		BlockedBy:           blocked,
		NeedsUserPermission: p.Rungs[c.Rung].NeedsUserPermission,
		Task:                st.Task,
	}, nil
}
