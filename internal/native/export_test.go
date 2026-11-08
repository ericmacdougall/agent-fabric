package native

import (
	"context"
	"time"

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

// NewKeyRing exposes the native key source and its redactor (goodqa#15087
// AC-5).
func NewKeyRing(argv []string, ttl time.Duration) (func(context.Context) (string, error), interface{ Redact(string) string }, error) {
	src, ring, err := newKeyRing(argv, ttl)
	if err != nil {
		return nil, nil, err
	}
	return src, ring, nil
}

// DefaultMonthSource is the production month source (goodqa#15124).
var DefaultMonthSource = monthSource

// SetMonthSource replaces the month cap's month source and returns the
// restore (goodqa#15124).
func SetMonthSource(f func(context.Context, *store.Store) (string, error)) func() {
	prev := monthSource
	monthSource = f
	return func() { monthSource = prev }
}

// StartNativeForTest exposes the keyed start/ack (goodqa#15235 S4).
func StartNativeForTest(ctx context.Context, st *store.Store, g Grant, admit bool, lockPath string) (int64, error) {
	return startNative(ctx, st, g, admit, lockPath)
}

// DisablePrecheck turns off native's own provider checks and returns the
// restore (goodqa#15087 AC-2 defense in depth).
func DisablePrecheck() func() {
	skipPrecheck = true
	return func() { skipPrecheck = false }
}

// SetHeartbeatFault installs the heartbeat test seam and returns its
// restore (goodqa#15087 AC-7).
func SetHeartbeatFault(f func() error) func() {
	prev := heartbeatFault
	heartbeatFault = f
	return func() { heartbeatFault = prev }
}
