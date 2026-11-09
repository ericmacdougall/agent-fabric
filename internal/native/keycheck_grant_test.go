package native_test

import (
	"strings"
	"testing"
)

// key_check is opt-in: empty, shape and authority are accepted, anything else
// is refused before a key is read, and the default stays shape.
func TestGrantKeyCheckModes(t *testing.T) {
	for mode, ok := range map[string]bool{"": true, "shape": true, "authority": true, "none": false, "SHAPE": false, "authority ": false} {
		g := capGrant(20_000_000, 5_000_000, 1_000_000)
		g.KeyCheck = mode
		err := g.Validate()
		if (err == nil) != ok {
			t.Errorf("key_check %q: err=%v, want ok=%v", mode, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "key_check") {
			t.Errorf("key_check %q: error does not name key_check: %v", mode, err)
		}
	}
}
