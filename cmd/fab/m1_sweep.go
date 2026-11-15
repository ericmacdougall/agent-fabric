package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"fabric/internal/credential/sweep"
	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/mission"
)

// sweepManager is the worker-manager surface the AC-16 sweep needs; the
// Linux worker manager (host.LinuxManager) provides it.
type sweepManager interface {
	StartIOWithProperties(g host.Grant, argv, env []string, props map[string]string, stdin, stdout, stderr *os.File) (host.Handle, error)
	Wait(h host.Handle) (int, error)
	Stop(h host.Handle, reason string, grace time.Duration) (host.StopReceipt, error)
}

// m1SweepOptions configure the per-settle sweep launcher (goodqa#15966
// AC-16, AC-35(h)).
type m1SweepOptions struct {
	mgr sweepManager
	// base carries the scheduler's grant policy: model, effort, rung, tier
	// table digest, profile, caps and wall.
	base        host.Grant
	streams     string // the fabd-only {run area}/streams
	attemptRoot string // {work root}/attempts: the M1 attempt's tree
	// fence reads the attempt's run record (its attempts row).
	fence func(domain.MissionID, domain.AttemptID) (int64, error)
}

// m1SweepFence reads the attempt's fence from its run record (the
// attempts row); an attempt the mission does not hold is refused.
func m1SweepFence(read func(domain.MissionID) (domain.Mission, error)) func(domain.MissionID, domain.AttemptID) (int64, error) {
	return func(m domain.MissionID, a domain.AttemptID) (int64, error) {
		ms, err := read(m)
		if err != nil {
			return 0, err
		}
		at, ok := ms.Attempts[a]
		if !ok || at.Fence <= 0 {
			return 0, fmt.Errorf("mission %s holds no attempt %s", m, a)
		}
		return at.Fence, nil
	}
}

// m1SweepFor is sched.Config.SweepFor. It rebuilds the attempt's grant from
// its run record (Run, Mission, Fence, Worktree, RunDir) with a fresh token,
// and returns a launcher over StartIOWithProperties with the sweep unit's
// limits. Any error leaves the settle sweep_incomplete.
func m1SweepFor(o m1SweepOptions) func(domain.MissionID, domain.AttemptID, string) (sweep.Launcher, error) {
	return func(m domain.MissionID, a domain.AttemptID, runDir string) (sweep.Launcher, error) {
		if o.mgr == nil || o.fence == nil || o.streams == "" {
			return nil, errors.New("sweep launcher not configured")
		}
		fence, err := o.fence(m, a)
		if err != nil {
			return nil, fmt.Errorf("run record of %s: %w", a, err)
		}
		var tok [16]byte
		if _, err := rand.Read(tok[:]); err != nil {
			return nil, err
		}
		g := o.base
		g.Run, g.Mission, g.Fence, g.RunDir = a, m, fence, runDir
		g.Worktree = filepath.Join(o.attemptRoot, string(a), mission.WorktreeDir)
		g.Token = "sweep-" + hex.EncodeToString(tok[:])
		if err := g.Validate(); err != nil {
			return nil, err
		}
		dir := filepath.Join(o.streams, string(a))
		return func(argv, env []string, stdin []byte) ([]byte, int, error) {
			return runSweepUnit(o.mgr, g, dir, argv, env, stdin)
		}, nil
	}
}

// sweepReportCap is one byte past sweep.Fabd's report limit, so an
// oversized report is still refused there.
const sweepReportCap = 1<<20 + 1

// runSweepUnit starts the sweep, waits for it and always stops it: only a
// verified Stop releases the worker manager for the next start. Its stdin
// and stdout are 0600 files in the attempt's fabd-only stream directory,
// unlinked once open, so no path to them outlives the call.
func runSweepUnit(mgr sweepManager, g host.Grant, dir string, argv, env []string, input []byte) ([]byte, int, error) {
	in, err := unlinkedFile(filepath.Join(dir, "sweep.stdin"), input, true)
	if err != nil {
		return nil, 0, err
	}
	defer in.Close()
	out, err := unlinkedFile(filepath.Join(dir, "sweep.stdout"), nil, false)
	if err != nil {
		return nil, 0, err
	}
	defer out.Close()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer null.Close()
	h, err := mgr.StartIOWithProperties(g, argv, env, sweep.UnitPropertyMap(), in, out, null)
	if err != nil {
		if h.Run != "" {
			mgr.Stop(h, "sweep start failed", 2*time.Second) //nolint:errcheck // the start error decides
		}
		return nil, 0, err
	}
	code, werr := mgr.Wait(h)
	rc, serr := mgr.Stop(h, "sweep exited; verify cleanup", 2*time.Second)
	switch {
	case werr != nil:
		return nil, 0, werr
	case serr != nil:
		return nil, 0, serr
	case !rc.Verified || len(rc.Survivors) > 0 || len(rc.Escaped) > 0:
		return nil, 0, errors.New("sweep unit stop not verified")
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	rep, err := io.ReadAll(io.LimitReader(out, sweepReportCap))
	return rep, code, err
}

// unlinkedFile creates path 0600 (replacing a leftover of an earlier
// settle) and writes data. It returns the file unlinked and at offset 0:
// read-only for stdin (the worker gets no writable descriptor to fabd's
// input), read-write for stdout, which fabd reads back.
func unlinkedFile(path string, data []byte, readOnly bool) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	w, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path) //nolint:errcheck // an early return still unlinks it
	_, err = w.Write(data)
	f := w
	if err == nil && readOnly {
		f, err = os.Open(path)
		w.Close()
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if rerr := os.Remove(path); err == nil && rerr != nil {
		err = rerr
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
