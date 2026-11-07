package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"fabric/internal/domain"
	"fabric/internal/hostid"
	"fabric/internal/native"
	"fabric/internal/sched"
	"fabric/internal/store"
)

// reconcileMain: `fab reconcile [--host H] [--worker-user U]` (goodqa#15087).
//
// One pass for a host with no fabd (such as the Linux live host): expire
// the host's lapsed leases, then quarantine its UNKNOWN attempts whose
// runner is provably gone, using the production probers (the stored lock
// path for native attempts, the cgroup readback for claude-code ones). It
// takes the host reconcile lock for the pass; a running fabd holds that
// lock for its whole life, so the pass is refused (exit 3, nothing
// written) while one runs. H must be this host: the probes read local
// state and cannot see a run on another machine (assumption A4).
func reconcileMain(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	h := fs.String("host", string(hostid.Default()), "host id to reconcile; must be this host")
	workerUser := fs.String("worker-user", os.Getenv("FABRIC_WORKER_USER"), "worker account whose cgroups hold claude-code runners (Linux)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("reconcile takes no arguments, got %q", fs.Args())}
	}
	host, local := domain.HostID(*h), hostid.Default()
	if host != local {
		// Refused before the database is opened.
		return usageError{fmt.Errorf("%w: this host is %s, asked for %s; run it on %s", sched.ErrWrongHost, local, host, host)}
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		var p sched.Prober
		if rp, ok := runnerProber(*workerUser); ok {
			p = rp
		}
		// L2: the pass logs a line naming FABRIC_ACTOR and counts in
		// reconcile_operator_passes_total.
		r, err := sched.OperatorReconcile(ctx, st, host, local, p, native.ProbeLock, actor(), os.Stderr)
		if errors.Is(err, store.ErrReconcileBusy) {
			return reconcileRefused{err}
		}
		for _, e := range r.Expired {
			fmt.Printf("expired %s (mission %s, fence %d) -> UNKNOWN\n", e.Attempt, e.Mission, e.Fence)
		}
		fmt.Printf("reconcile %s: expired %d, quarantined %d\n", host, len(r.Expired), r.Quarantined)
		return err
	})
}

// reconcileRefused: another reconciler holds the host; nothing was written
// (exit 3).
type reconcileRefused struct{ error }

func (e reconcileRefused) Unwrap() error { return e.error }
