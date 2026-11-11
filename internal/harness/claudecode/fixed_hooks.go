package claudecode

import (
	"encoding/json"
	"errors"
	"regexp"
)

// FixedRequiredHookCounts derives the selected single-Write lifecycle from the
// same hash-bound settings snapshot passed to the native client. All declared
// hooks remain installed; unmatched shell/subagent hooks are not invented runs.
func FixedRequiredHookCounts(settings []byte) (map[string]int, error) {
	var value struct {
		Hooks map[string][]struct {
			Matcher string            `json:"matcher"`
			Hooks   []json.RawMessage `json:"hooks"`
		} `json:"hooks"`
	}
	if strictJSON(settings, &value) != nil {
		return nil, errors.New("claudecode: hook requirements unavailable")
	}
	result := map[string]int{}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"} {
		for _, row := range value.Hooks[event] {
			if event == "PreToolUse" && row.Matcher != "" {
				r, e := regexp.Compile(row.Matcher)
				if e != nil {
					return nil, errors.New("claudecode: unsupported hook matcher")
				}
				if !r.MatchString("Write") {
					continue
				}
			}
			result[event] += len(row.Hooks)
		}
		if result[event] == 0 {
			return nil, errors.New("claudecode: required lifecycle hooks missing")
		}
	}
	return result, nil
}

type fixedHookIdentity struct{ event, name string }
type fixedHookState struct {
	required map[string]int
	started  map[string]fixedHookIdentity
	finished map[string]bool
	counts   map[string]int
	starts   int
	progress int
}

func newFixedHookState(required map[string]int) (*fixedHookState, error) {
	h := &fixedHookState{required: map[string]int{}, started: map[string]fixedHookIdentity{}, finished: map[string]bool{}, counts: map[string]int{}}
	if len(required) != 4 {
		return nil, errors.New("claudecode: selected lifecycle requirements missing")
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"} {
		n := required[event]
		if n < 1 || n > 32 {
			return nil, errors.New("claudecode: invalid lifecycle requirement")
		}
		h.required[event] = n
	}
	return h, nil
}

func (h *fixedHookState) observe(phase, id, event, name string, exit *int, outcome string) bool {
	if h == nil || !controlID.MatchString(id) || h.required[event] == 0 || name == "" || len(name) > 1024 {
		return false
	}
	identity := fixedHookIdentity{event: event, name: name}
	switch phase {
	case "hook_started":
		if _, exists := h.started[id]; exists || h.finished[id] {
			return false
		}
		h.started[id] = identity
		h.starts++
		return true
	case "hook_progress":
		prior, exists := h.started[id]
		if !exists || prior != identity || h.finished[id] {
			return false
		}
		h.progress++
		return true
	case "hook_response":
		prior, exists := h.started[id]
		if !exists || prior != identity || h.finished[id] || exit == nil || *exit != 0 || outcome != "success" {
			return false
		}
		delete(h.started, id)
		h.finished[id] = true
		h.counts[event]++
		return h.counts[event] <= h.required[event]
	}
	return false
}

func (h *fixedHookState) readyForWrite() bool {
	if h == nil || len(h.started) != 0 {
		return false
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse"} {
		if h.counts[event] != h.required[event] {
			return false
		}
	}
	return true
}
func (h *fixedHookState) complete() bool {
	if !h.readyForWrite() {
		return false
	}
	return h.counts["Stop"] == h.required["Stop"]
}
