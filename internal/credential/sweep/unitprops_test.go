package sweep

import (
	"reflect"
	"sort"
	"testing"

	"fabric/internal/host"
)

// AC-16 (8): the sweep's unit properties are exactly the worker manager's
// allowlist, and every value passes it (host cannot import this package).
func TestUnitPropertiesAllowlisted(t *testing.T) {
	m := UnitPropertyMap()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, host.UnitPropertyKeys()) {
		t.Fatalf("sweep property keys %v, host allowlist %v", keys, host.UnitPropertyKeys())
	}
	args, err := host.UnitPropertyArgs(m)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), UnitProperties...)
	sort.Strings(want)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args %v, want %v", args, want)
	}
}
