package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// FileEditCommand emits fixed invocation-only overrides. It intentionally
// accepts no caller config, command tail, environment or profile name.
func FileEditCommand(bin, profile, dir string) ([]string, error) {
	if !filepath.IsAbs(bin) || !filepath.IsAbs(dir) {
		return nil, errors.New("codex: absolute executable and workspace required")
	}
	c := Control{Profile: profile, InstructionSources: []string{"required"}}
	if err := c.validate(); err != nil {
		return nil, err
	}
	argv := []string{bin}
	var add func(string, map[string]any)
	add = func(prefix string, m map[string]any) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if child, ok := m[k].(map[string]any); ok && len(child) > 0 {
				add(key, child)
				continue
			}
			b, _ := json.Marshal(m[k])
			argv = append(argv, "-c", key+"="+string(b))
		}
	}
	add("", FileEditSettings())
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	prefix := "permissions." + profile
	argv = append(argv, "-c", prefix+".extends=\":workspace\"", "-c", prefix+".filesystem={\":root\"=\"deny\",\":minimal\"=\"read\",\":tmpdir\"=\"deny\",\":slash_tmp\"=\"deny\","+q(dir)+"=\"write\"}", "-c", prefix+".network.enabled=false")
	argv = append(argv, "app-server")
	// Guard accidental changes to the fixed command builder.
	for _, a := range argv[1:] {
		if strings.Contains(a, "danger-full-access") {
			return nil, fmt.Errorf("codex: unsafe command configuration")
		}
	}
	return argv, nil
}
