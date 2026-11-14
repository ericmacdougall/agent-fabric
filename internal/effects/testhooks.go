package effects

import (
	"testing"
	"time"
)

// TestHooks sets the broker's quiet window and crash cutpoints for tests in
// other packages (goodqa#15966 AC-8). It does nothing outside a test binary
// (testing.Testing), so no production caller can shorten the AC-7 guard.
// It reports whether it applied.
func (b *Broker) TestHooks(quietWindow time.Duration, afterDispatchCommit func(Effect), afterTransport func(Effect, Receipt)) bool {
	if !testing.Testing() {
		return false
	}
	b.quietWindow = quietWindow
	b.afterDispatchCommit = afterDispatchCommit
	b.afterTransport = afterTransport
	return true
}
