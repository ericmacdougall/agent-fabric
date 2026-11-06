// Package projection builds the model-visible view of a mission (DESIGN.md
// 2.5 "context projection", AC-18): a compact checkpoint plus a bounded
// window of recent events, with a hard token cap. It fails closed when the
// checkpoint alone exceeds its budget, and an unresolved UNKNOWN effect is
// always present, however old. The output is deterministic and digested so
// two hosts building from the same state agree byte for byte.
package projection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"fabric/internal/domain"
)

const (
	BuilderVersion     = "proj-1"
	CheckpointMaxBytes = 2048 // DESIGN.md 3.4: the checkpoint is at most 2 KB
	DefaultCapTokens   = 6000 // 4-8k window (DESIGN.md 2.5)
)

var (
	ErrBudget = errors.New("projection budget fault: the checkpoint alone exceeds its cap")
)

// Checkpoint is the state part of the projection.
type Checkpoint struct {
	Mission   domain.MissionID `json:"mission"`
	Version   domain.Version   `json:"version"`
	Execution string           `json:"execution"`
	Candidate string           `json:"candidate"`
	Delivery  string           `json:"delivery"`
	Head      string           `json:"head,omitempty"`
	WaitKey   string           `json:"wait_key,omitempty"`
	Unknown   []string         `json:"unknown_effects,omitempty"` // always retained
	Accepted  *string          `json:"accepted_attempt,omitempty"`
	LiveRuns  []string         `json:"live_attempts,omitempty"`
	Next      string           `json:"next,omitempty"`
}

// Projection is what a worker gets as its prompt material.
type Projection struct {
	Builder    string     `json:"builder"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Window     []string   `json:"window"` // newest last, each a one-line rendering of an event
	Tokens     int        `json:"tokens"`
	Dropped    int        `json:"dropped"` // events that did not fit the cap
	Digest     string     `json:"digest"`
}

// Tokens estimates tokens as bytes/4 (the N4 convention; a size bound, not a
// tokenizer).
func Tokens(b []byte) int { return (len(b) + 3) / 4 }

// Build composes the projection for a mission from its state, the full event
// log (oldest first) and the cap in tokens. The checkpoint comes first and
// must fit CheckpointMaxBytes; the window fills with the newest events until
// the cap, oldest dropped first, and the count of dropped events is reported.
func Build(m domain.Mission, events []domain.Event, capTokens int, next string) (Projection, error) {
	if capTokens <= 0 {
		capTokens = DefaultCapTokens
	}
	cp := Checkpoint{Mission: m.ID, Version: m.Version, Execution: string(m.Execution), Candidate: string(m.Candidate),
		Delivery: string(m.Delivery), Head: m.Head, WaitKey: m.WaitKey, Next: next}
	for id, f := range m.Effects {
		if f.State == domain.EffectUnknown {
			cp.Unknown = append(cp.Unknown, string(id))
		}
	}
	sort.Strings(cp.Unknown)
	for id, a := range m.Attempts {
		if a.State.Live() {
			cp.LiveRuns = append(cp.LiveRuns, string(id))
		}
	}
	sort.Strings(cp.LiveRuns)
	if m.Accepted != nil {
		s := string(m.Accepted.Attempt)
		cp.Accepted = &s
	}
	cpJSON, err := json.Marshal(cp)
	if err != nil {
		return Projection{}, err
	}
	if len(cpJSON) > CheckpointMaxBytes {
		return Projection{}, fmt.Errorf("%w: %d bytes", ErrBudget, len(cpJSON))
	}
	p := Projection{Builder: BuilderVersion, Checkpoint: cp, Tokens: Tokens(cpJSON)}
	if p.Tokens > capTokens {
		return Projection{}, fmt.Errorf("%w: %d tokens > cap %d", ErrBudget, p.Tokens, capTokens)
	}
	// Window: newest first until the cap, then reversed to chronological order.
	var window []string
	for i := len(events) - 1; i >= 0; i-- {
		line := render(events[i])
		t := Tokens([]byte(line)) + 1
		if p.Tokens+t > capTokens {
			p.Dropped = i + 1
			break
		}
		p.Tokens += t
		window = append(window, line)
	}
	for i, j := 0, len(window)-1; i < j; i, j = i+1, j-1 {
		window[i], window[j] = window[j], window[i]
	}
	p.Window = window
	if p.Window == nil {
		p.Window = []string{}
	}
	body, err := json.Marshal(struct {
		Builder    string     `json:"builder"`
		Checkpoint Checkpoint `json:"checkpoint"`
		Window     []string   `json:"window"`
	}{p.Builder, p.Checkpoint, p.Window})
	if err != nil {
		return Projection{}, err
	}
	sum := sha256.Sum256(body)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}

func render(e domain.Event) string {
	s := fmt.Sprintf("#%d %s", e.Seq, e.Kind)
	if e.Attempt != "" {
		s += " " + string(e.Attempt)
	}
	if e.Outcome != "" {
		s += " " + e.Outcome
	}
	if e.Effect != "" {
		s += " " + string(e.Effect)
	}
	if e.Result != "" {
		s += " " + e.Result
	}
	if e.Reason != "" {
		r := e.Reason
		if len(r) > 120 {
			r = r[:120]
		}
		s += ": " + r
	}
	return s
}
