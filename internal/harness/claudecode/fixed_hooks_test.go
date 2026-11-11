package claudecode

import "testing"

func TestFixedHooksBindCompletionAndRequiredPhases(t *testing.T) {
	expected := map[string]int{"SessionStart": 1, "UserPromptSubmit": 1, "PreToolUse": 1, "Stop": 1}
	zero := 0
	h, err := newFixedHookState(expected)
	if err != nil {
		t.Fatal(err)
	}
	if h.complete() || h.readyForWrite() {
		t.Fatal("missing hooks accepted")
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse"} {
		id := "h_" + event
		if !h.observe("hook_started", id, event, event, nil, "") || !h.observe("hook_response", id, event, event, &zero, "success") {
			t.Fatal("valid lifecycle refused")
		}
	}
	if !h.readyForWrite() || h.complete() {
		t.Fatal("wrong pre-write/stop state")
	}
	if !h.observe("hook_started", "stop", "Stop", "Stop", nil, "") {
		t.Fatal("stop start")
	}
	if h.complete() {
		t.Fatal("outstanding hook accepted")
	}
	if !h.observe("hook_response", "stop", "Stop", "Stop", &zero, "success") || !h.complete() {
		t.Fatal("complete lifecycle missing")
	}
	if h.observe("hook_response", "stop", "Stop", "Stop", &zero, "success") {
		t.Fatal("duplicate response accepted")
	}
	for _, tc := range []struct {
		name, id, event, hook, outcome string
		exit                           *int
	}{
		{"unbound", "other", "Stop", "Stop", "success", &zero},
		{"wrongEvent", "h", "PreToolUse", "Stop", "success", &zero},
		{"wrongName", "h", "Stop", "other", "success", &zero},
		{"missingExit", "h", "Stop", "Stop", "success", nil},
		{"failedOutcome", "h", "Stop", "Stop", "non_blocking_error", &zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newFixedHookState(expected)
			h.observe("hook_started", "h", "Stop", "Stop", nil, "")
			if h.observe("hook_response", tc.id, tc.event, tc.hook, tc.exit, tc.outcome) {
				t.Fatal("invalid completion accepted")
			}
		})
	}
}

func TestFixedRequiredHookCountsSelectedWrite(t *testing.T) {
	input := []byte(`{"hooks":{"SessionStart":[{"hooks":[{},{}]}],"UserPromptSubmit":[{"hooks":[{}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{}]},{"matcher":"Write|Edit","hooks":[{},{}]}],"Stop":[{"hooks":[{}]}]}}`)
	count, err := FixedRequiredHookCounts(input)
	if err != nil || count["SessionStart"] != 2 || count["PreToolUse"] != 2 || len(count) != 4 {
		t.Fatalf("wrong selected counts %v %v", count, err)
	}
	if _, err := FixedRequiredHookCounts([]byte(`{"hooks":{}}`)); err == nil {
		t.Fatal("missing required hooks")
	}
}
