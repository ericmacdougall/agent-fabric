package claude

import (
	"errors"
	"path/filepath"
	"testing"
)

// goodqa#15966: a hook may carry onFailure "block" (a gate that cannot run
// blocks the call); any other onFailure value is refused.
func TestHookOnFailureOnlyBlock(t *testing.T) {
	dir, script := hooksDir(t)
	settings := filepath.Join(t.TempDir(), "settings.json")
	hook := func(v any) map[string]any {
		e := map[string]any{"type": "command", "command": script, "timeout": 30}
		if v != nil {
			e["onFailure"] = v
		}
		return map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{e}}}}}
	}
	for _, c := range []struct {
		v  any
		ok bool
	}{{nil, true}, {"block", true}, {"continue", false}, {"Block", false}, {true, false}, {"", false}} {
		writeJSON(t, settings, hook(c.v))
		err := CheckSettingsFile(settings, dir, nil)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrSettings)) {
			t.Errorf("onFailure %v: %v, want ok=%v", c.v, err, c.ok)
		}
	}
}
