// Package ladder is the model ladder as a pure function of receipts
// (DESIGN.md 3.8, R10; ported from escalation.py's rules). It knows rung
// names and their order, never model names: the tier table maps rungs to
// models at run time and is read by the daemon, not by this package (R23).
package ladder

import (
	"errors"
	"fmt"
)

// Rung is one step of the table. Reviewer is the rung that reviews work
// written at this rung.
type Rung struct {
	Name            string
	Writable        bool
	Reviewer        string
	NeedsPermission bool
}

// Table is the ordered ladder.
type Table struct {
	Rungs           []Rung // lowest first
	FailuresPerRung int    // genuine failures before the writer moves up
}

// EventKind classifies a receipt for the ladder.
type EventKind string

const (
	ReviewFail EventKind = "review_fail" // a reviewer verdict FAIL on a changed head
	TestFail   EventKind = "test_fail"   // a test receipt failing after a claimed fix
	Infra      EventKind = "infra"       // provider 401/429/5xx, host fault, interrupted run: never counts
	NewFinding EventKind = "new_finding" // a different material finding resets the count
	Reset      EventKind = "reset"       // an explicit reset recorded by the main agent
)

// Event is one ladder-relevant receipt. Head binds a failure to the code it
// judged: a second FAIL on the same head is the same failure, not a new one.
type Event struct {
	Kind EventKind
	Head string
}

// State is the resolved position.
type State struct {
	Rung     string
	Failures int    // genuine failures counted at the current rung
	Stopped  bool   // the next rung needs a permission that was not granted
	Reason   string // why it stopped, when it did
}

var ErrUnknownRung = errors.New("unknown rung")

func (t Table) index(name string) int {
	for i, r := range t.Rungs {
		if r.Name == name {
			return i
		}
	}
	return -1
}

// Lowest is where an unknown or unset rung starts (CLAUDE.md: reset to the
// lowest rung and record why).
func (t Table) Lowest() string {
	if len(t.Rungs) == 0 {
		return ""
	}
	return t.Rungs[0].Name
}

// Reviewer returns the reviewer rung for a writer rung.
func (t Table) Reviewer(writer string) (string, error) {
	i := t.index(writer)
	if i < 0 {
		return "", fmt.Errorf("%w: %q", ErrUnknownRung, writer)
	}
	return t.Rungs[i].Reviewer, nil
}

// Resolve folds the events from a starting rung. granted names the rungs a
// user-sourced ask has enabled (R23: the top rung is absent unless granted).
// Infrastructure events never count; a new finding resets the count; a
// repeated failure on an unchanged head is not a second failure.
func Resolve(t Table, start string, events []Event, granted map[string]bool) (State, error) {
	if t.FailuresPerRung <= 0 {
		return State{}, errors.New("ladder: failures_per_rung must be positive")
	}
	if start == "" || t.index(start) < 0 {
		start = t.Lowest()
	}
	s := State{Rung: start}
	lastFailHead := ""
	for _, e := range events {
		switch e.Kind {
		case Infra:
			continue
		case NewFinding, Reset:
			s.Failures = 0
			lastFailHead = ""
			continue
		case ReviewFail, TestFail:
			if e.Head != "" && e.Head == lastFailHead {
				continue // same head, same failure
			}
			lastFailHead = e.Head
			s.Failures++
		default:
			return State{}, fmt.Errorf("ladder: unknown event kind %q", e.Kind)
		}
		if s.Failures >= t.FailuresPerRung {
			next, stop, why := t.next(s.Rung, granted)
			if stop {
				s.Stopped = true
				s.Reason = why
				return s, nil
			}
			s.Rung = next
			s.Failures = 0
			lastFailHead = ""
		}
	}
	return s, nil
}

// next finds the next writable rung above cur, honouring permissions.
func (t Table) next(cur string, granted map[string]bool) (string, bool, string) {
	for i := t.index(cur) + 1; i < len(t.Rungs); i++ {
		r := t.Rungs[i]
		if !r.Writable {
			continue
		}
		if r.NeedsPermission && !granted[r.Name] {
			return "", true, fmt.Sprintf("rung %s needs a user-granted permission that is absent; stop and ask", r.Name)
		}
		return r.Name, false, ""
	}
	return "", true, fmt.Sprintf("no writable rung above %s; stop and report", cur)
}
