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
func FileEditCommand(bin, profile, dir string, disabled ...DisabledIntegration) ([]string, error) {
	if !filepath.IsAbs(bin) || !filepath.IsAbs(dir) {
		return nil, errors.New("codex: absolute executable and workspace required")
	}
	c := Control{Profile: profile, InstructionSources: []string{"required"}}
	if err := c.validate(); err != nil {
		return nil, err
	}
	argv := []string{bin, "--strict-config"}
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
	entries := []string{}
	rules := FileEditPermissions(dir)["filesystem"].(map[string]any)
	for k, v := range rules {
		entries = append(entries, q(k)+"="+q(v.(string)))
	}
	sort.Strings(entries)
	argv = append(argv, "-c", prefix+".filesystem={"+strings.Join(entries, ",")+"}", "-c", prefix+".network.enabled=false")
	tables := map[string]map[string]bool{}
	for _, d := range disabled {
		if d.Kind != "mcp_servers" && d.Kind != "plugins" {
			return nil, errors.New("codex: unsupported integration kind")
		}
		if d.ID == "" || len(d.ID) > 200 || strings.ContainsAny(d.ID, "\x00\n\r") {
			return nil, errors.New("codex: invalid integration identifier")
		}
		if tables[d.Kind] == nil {
			tables[d.Kind] = map[string]bool{}
		}
		tables[d.Kind][d.ID] = true
	}
	for _, kind := range []string{"mcp_servers", "plugins"} {
		ids := make([]string, 0, len(tables[kind]))
		for id := range tables[kind] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			continue
		}
		entries := make([]string, 0, len(ids))
		for _, id := range ids {
			entries = append(entries, q(id)+"={enabled=false}")
		}
		argv = append(argv, "-c", kind+"={"+strings.Join(entries, ",")+"}")
	}
	argv = append(argv, "app-server")
	// Guard accidental changes to the fixed command builder.
	for _, a := range argv[1:] {
		if strings.Contains(a, "danger-full-access") {
			return nil, fmt.Errorf("codex: unsafe command configuration")
		}
	}
	return argv, nil
}

// DisabledIntegration can only remove an inherited registration. IDs become
// quoted keys inside a TOML value, never dotted override paths (which this
// Codex version splits literally, without TOML key unquoting).
type DisabledIntegration struct {
	Kind string
	ID   string
}
