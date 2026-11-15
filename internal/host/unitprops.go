package host

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// ErrUnitProperty refuses a per-unit property outside the allowlist, or a
// malformed value, before any unit starts (goodqa#15966 AC-16 (8), AC-35(h)).
var ErrUnitProperty = errors.New("unit property refused")

var (
	unitSeconds = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	unitBytes   = regexp.MustCompile(`^[1-9][0-9]{0,9}[KMG]?$`)
)

// unitPropertyRules is the allowlist: the sweep unit's limits and nothing
// else. LimitCORE may only be 0, so no caller can undo the worker's no-core
// rule (goodqa#15976 AC-5).
var unitPropertyRules = map[string]func(string) bool{
	"RuntimeMaxSec": func(v string) bool { n, _ := strconv.Atoi(v); return unitSeconds.MatchString(v) && n <= 86400 },
	"MemoryMax":     unitBytes.MatchString,
	"LimitCORE":     func(v string) bool { return v == "0" },
}

// UnitPropertyKeys lists the allowed property names, sorted.
func UnitPropertyKeys() []string {
	keys := make([]string, 0, len(unitPropertyRules))
	for k := range unitPropertyRules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// UnitPropertyArgs validates props and returns them as NAME=VALUE, sorted by
// name. Any key outside the allowlist, or a malformed value, is refused.
func UnitPropertyArgs(props map[string]string) ([]string, error) {
	out := make([]string, 0, len(props))
	for k, v := range props {
		ok, known := unitPropertyRules[k]
		if !known {
			return nil, fmt.Errorf("%w: %q is not an allowed unit property", ErrUnitProperty, k)
		}
		if !ok(v) {
			return nil, fmt.Errorf("%w: malformed value for %s", ErrUnitProperty, k)
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}
