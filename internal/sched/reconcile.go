package sched

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// LockProber reads a native (anthropic-api) attempt back through the flock
// on the lock path stored in its row (goodqa#15087): true only while a live
// process holds it. cmd/fab injects native.ProbeLock; sched cannot import
// native, whose tests import sched.
type LockProber func(attempt domain.AttemptID, lockPath string) (alive bool)

// SetLockProber installs the native readback the UNKNOWN reconciler uses
// for anthropic-api attempts. Claude-code attempts keep the Prober.
func (s *Scheduler) SetLockProber(p LockProber) { s.lockProber = p }

// RequireReconcileLock makes this scheduler act on its host's attempts
// (lease expiry and the UNKNOWN reconcile) only while it holds the host
// reconcile lock (goodqa#15087). fabd turns it on and holds the lock for
// its whole life, so a concurrent `fab reconcile` is always refused.
// Without it, Tick behaves as before (tests that drive Tick directly).
func (s *Scheduler) RequireReconcileLock() { s.lockRequired = true }

// Close releases the host reconcile lock, if held.
func (s *Scheduler) Close() {
	s.hostLock.Release()
	s.hostLock = nil
}

// HoldsReconcileLock reports whether the scheduler holds its host lock.
func (s *Scheduler) HoldsReconcileLock() bool { return s.hostLock != nil }

// reconcileAllowed: true when the scheduler may expire leases and
// reconcile this tick. With the lock required, it re-checks a held lock's
// connection (a lost connection loses the lock), and otherwise tries to
// take the lock; until it holds it, nothing is reconciled.
func (s *Scheduler) reconcileAllowed(ctx context.Context) bool {
	if !s.lockRequired {
		return true
	}
	if s.hostLock != nil {
		if err := s.hostLock.Check(ctx); err == nil {
			return true
		}
		fmt.Fprintf(os.Stderr, "fabd: lost reconcile lock for host %s; not reconciling until it is held again\n", s.cfg.Host)
		s.Close()
	}
	l, err := s.st.TryReconcileLock(ctx, s.cfg.Host)
	if err != nil {
		if !s.lockWaitLogged {
			fmt.Fprintf(os.Stderr, "fabd: waiting for the reconcile lock for host %s: %v\n", s.cfg.Host, err)
			s.lockWaitLogged = true
		}
		return false
	}
	s.hostLock, s.lockWaitLogged = l, false
	fmt.Fprintf(os.Stderr, "fabd: holding reconcile lock for host %s\n", s.cfg.Host)
	return true
}

// ReconcileResult is what one `fab reconcile` pass did.
type ReconcileResult struct {
	Expired     []store.ExpiredLease
	Quarantined int
}

// ErrWrongHost: `fab reconcile` is asked for a host other than the one it
// runs on. Its probes read local files and cgroups, which cannot see a run
// on another machine, so it refuses instead of guessing (assumption A4).
var ErrWrongHost = errors.New("fab reconcile runs only on the host it reconciles")

// ReconcileHost is `fab reconcile`'s one pass for host: one expire-leases
// pass limited to host's attempts, then one UNKNOWN reconcile pass for host,
// both under host's reconcile lock (goodqa#15087). local is the host this
// process runs on; any other host is refused before the database is
// touched. A held lock is refused with store.ErrReconcileBusy and nothing
// written.
func ReconcileHost(ctx context.Context, st *store.Store, host, local domain.HostID, p Prober, lp LockProber) (ReconcileResult, error) {
	var r ReconcileResult
	if host != local {
		return r, fmt.Errorf("%w: this host is %s, asked for %s; run it on %s", ErrWrongHost, local, host, host)
	}
	l, err := st.TryReconcileLock(ctx, host)
	if err != nil {
		return r, err
	}
	defer l.Release()
	exp, err := st.ExpireLeasesOnHost(ctx, host, 50)
	r.Expired = exp
	if err != nil {
		fmt.Fprintln(os.Stderr, "fab reconcile: lease sweep:", err)
	}
	s := &Scheduler{st: st, cfg: Config{Host: host}, prober: p, lockProber: lp}
	r.Quarantined = s.reconcileUnknown(ctx)
	return r, err
}
