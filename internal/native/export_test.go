package native

import (
	"fabric/internal/settle"
	"fabric/internal/store"
)

// SetFinishSeams installs the finish-retry test seams and returns their
// restore (goodqa#15087 AC-7).
func SetFinishSeams(after func(store.Response, error) error, clock settle.Clock) func() {
	prevAfter, prevClock := afterFinishExec, settleClock
	afterFinishExec, settleClock = after, clock
	return func() { afterFinishExec, settleClock = prevAfter, prevClock }
}

// SetHeartbeatFault installs the heartbeat test seam and returns its
// restore (goodqa#15087 AC-7).
func SetHeartbeatFault(f func() error) func() {
	prev := heartbeatFault
	heartbeatFault = f
	return func() { heartbeatFault = prev }
}
