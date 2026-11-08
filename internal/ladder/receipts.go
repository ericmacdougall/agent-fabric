package ladder

// goodqa#15324 S1b: the fold from stored ladder events into the unchanged
// Replay/Status. escalation.py classified every record; nothing here derives a
// class. Replay equals live by construction: both are Fold over the same log.

import (
	"encoding/json"
	"fmt"
	"sort"

	"fabric/internal/domain"
)

// MaxReceiptsPerTask is escalation.py MAX_EVENTS. Only LADDER_RECEIPT events
// count (approvals have their own, smaller bound), on write and on read.
const MaxReceiptsPerTask = domain.MaxLadderReceipts

// FPCount is the count of FAIL events for one fingerprint, and per head.
type FPCount struct {
	Fingerprint string         `json:"fingerprint"`
	Fails       int            `json:"fails"`
	Heads       map[string]int `json:"heads,omitempty"`
	IDs         []string       `json:"ids,omitempty"` // named finding ids of the latest FAIL (display only)
}

// Folded is everything the status needs from a ladder log.
type Folded struct {
	Ledger      []LedgerEvent
	Launches    int
	SpendMicros int64
	Receipts    int
	Approved    bool
	Approvals   []domain.LadderApproval
	ByFP        []FPCount
}

// Fold maps a ladder mission's log. It refuses a log over the cap, a foreign
// kind and any record that fails the shape rules; it never truncates.
func Fold(evs []domain.Event) (Folded, error) {
	var f Folded
	byFP := map[string]*FPCount{}
	for _, e := range evs {
		switch e.Kind {
		case domain.EvLadderTopApproved:
			if err := domain.ValidateLadderRecord(e.Ladder); err != nil || e.Ladder.Step != "approve_top" {
				return Folded{}, fmt.Errorf("%w: stored approval is malformed", domain.ErrInvalidReceipt)
			}
			if len(f.Approvals) >= domain.MaxLadderApprovals {
				return Folded{}, fmt.Errorf("%w: more than %d approvals", domain.ErrTooManyReceipts, domain.MaxLadderApprovals)
			}
			f.Approved = true
			f.Approvals = append(f.Approvals, *e.Ladder.Approval)
		case domain.EvLadderReceipt:
			if err := domain.ValidateLadderRecord(e.Ladder); err != nil || e.Ladder.Step == "approve_top" {
				return Folded{}, fmt.Errorf("%w: stored receipt is malformed", domain.ErrInvalidReceipt)
			}
			if f.Receipts >= MaxReceiptsPerTask {
				return Folded{}, fmt.Errorf("%w: more than %d receipts", domain.ErrTooManyReceipts, MaxReceiptsPerTask)
			}
			f.Receipts++
			r := e.Ladder
			f.Ledger = append(f.Ledger, LedgerEvent{Class: r.Class, Fingerprint: r.Fp, Head: r.Commit})
			if r.Counted {
				f.Launches++
			}
			f.SpendMicros += r.SpendMicros
			if r.Class == "FAIL" {
				c := byFP[*r.Fp]
				if c == nil {
					c = &FPCount{Fingerprint: *r.Fp, Heads: map[string]int{}}
					byFP[*r.Fp] = c
				}
				c.Fails++
				if r.Commit != nil {
					c.Heads[*r.Commit]++
				}
				c.IDs = append([]string(nil), r.IDs...)
			}
		default:
			return Folded{}, fmt.Errorf("%w: %s in a ladder log", domain.ErrUnsupportedEvent, e.Kind)
		}
	}
	for _, c := range byFP {
		f.ByFP = append(f.ByFP, *c)
	}
	sort.Slice(f.ByFP, func(i, j int) bool {
		if f.ByFP[i].Fails != f.ByFP[j].Fails {
			return f.ByFP[i].Fails > f.ByFP[j].Fails
		}
		return f.ByFP[i].Fingerprint < f.ByFP[j].Fingerprint
	})
	if len(f.ByFP) > 50 {
		f.ByFP = f.ByFP[:50]
	}
	return f, nil
}

// FromEvents is the ledger half of Fold.
func FromEvents(evs []domain.Event) ([]LedgerEvent, error) {
	f, err := Fold(evs)
	if err != nil {
		return nil, err
	}
	return f.Ledger, nil
}

// TaskStatus is the status of one task derived from its Fabric log.
type TaskStatus struct {
	LadderStatus
	ReviewerRung string    `json:"reviewer_rung"`
	NextRung     string    `json:"next_rung"`
	Decision     string    `json:"decision"` // CONTINUE | STOP_AND_ASK | STOP
	Advisory     bool      `json:"advisory"` // always true until S3 (goodqa#15135); the hook is the enforcer
	Approved     bool      `json:"top_approved"`
	Launches     int       `json:"launches"`
	SpendMicros  int64     `json:"spend_micros"`
	Receipts     int       `json:"receipts"`
	ByFP         []FPCount `json:"by_fingerprint"`
}

// MicrosText renders micros as a decimal USD string for pyNumber.
func MicrosText(m int64) json.Number {
	return json.Number(fmt.Sprintf("%d.%06d", m/1_000_000, m%1_000_000))
}

// StatusOf folds the log and applies the existing Status. NEG-3: a rung that
// needs user permission is STOP_AND_ASK unless an approval exists for this
// task (the mission id binds it). A climb or cost limit is STOP.
func StatusOf(p Policy, task string, evs []domain.Event) (TaskStatus, error) {
	f, err := Fold(evs)
	if err != nil {
		return TaskStatus{}, err
	}
	st, err := Status(p, LedgerState{Task: task, Events: f.Ledger, Launches: f.Launches, SpendUSD: MicrosText(f.SpendMicros)})
	if err != nil {
		return TaskStatus{}, err
	}
	next, _ := p.nextWritingRung(st.Rung)
	out := TaskStatus{LadderStatus: st, ReviewerRung: p.Rungs[st.Rung].ReviewerRung, NextRung: next, Advisory: true,
		Approved: f.Approved, Launches: f.Launches, SpendMicros: f.SpendMicros, Receipts: f.Receipts, ByFP: f.ByFP, Decision: "CONTINUE"}
	if out.ByFP == nil {
		out.ByFP = []FPCount{}
	}
	switch {
	case len(st.BlockedBy) > 0:
		out.Decision = "STOP"
	case st.NeedsUserPermission && !f.Approved:
		out.Decision = "STOP_AND_ASK"
	}
	return out, nil
}
