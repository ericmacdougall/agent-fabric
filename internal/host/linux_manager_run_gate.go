package host

import "sync/atomic"

// runGate serializes one Linux profile from Start until verified Stop. Wait
// deliberately does not open the gate: leader exit is not verified cleanup.
type runGate struct {
	held atomic.Bool
}

func (g *runGate) tryLock() bool {
	return g.held.CompareAndSwap(false, true)
}

func (g *runGate) unlock() {
	g.held.Store(false)
}
