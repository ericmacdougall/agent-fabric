package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"

	"fabric/internal/domain"
)

// ReconcileLockNamespace is the first key of the host reconcile lock
// (goodqa#15087). The two-key form pg_try_advisory_lock(int4, int4) lives
// in a different lock space from the single-key migration lock and the
// agent-run locks, so none of them can collide with it.
const ReconcileLockNamespace int32 = 0x46616272 // "Fabr"

// ErrReconcileBusy: another reconciler (a running fabd, or another
// `fab reconcile`) holds the host's reconcile lock.
var ErrReconcileBusy = errors.New("another reconciler holds host")

// ReconcileLockKeys returns the two keys of host's reconcile lock: the
// fixed namespace and a 32-bit FNV-1a hash of the host id. Two hosts whose
// ids hash alike only refuse each other's reconcile; they never act at once.
func ReconcileLockKeys(host domain.HostID) (int32, int32) {
	h := fnv.New32a()
	h.Write([]byte(host)) //nolint:errcheck // hash writes never fail
	return ReconcileLockNamespace, int32(h.Sum32())
}

// HostLock is a held host reconcile lock. It owns a dedicated connection
// taken out of the pool, so the session lock lives exactly as long as that
// connection: Release closes it, and a lost connection loses the lock.
type HostLock struct {
	Host domain.HostID
	conn *pgx.Conn
}

// TryReconcileLock takes host's reconcile lock without waiting. It returns
// ErrReconcileBusy, naming the host, when another session holds it.
func (s *Store) TryReconcileLock(ctx context.Context, host domain.HostID) (*HostLock, error) {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// Hijack: the pool must never hand this session (and its lock) to
	// anyone else, nor reset it behind our back.
	conn := pc.Hijack()
	k1, k2 := ReconcileLockKeys(host)
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1::int4, $2::int4)", k1, k2).Scan(&ok); err != nil {
		conn.Close(context.Background()) //nolint:errcheck
		return nil, err
	}
	if !ok {
		conn.Close(context.Background()) //nolint:errcheck
		return nil, fmt.Errorf("%w %s", ErrReconcileBusy, host)
	}
	return &HostLock{Host: host, conn: conn}, nil
}

// Check reports whether the lock's connection still answers. An error
// means the session, and with it the lock, may be gone: the caller must
// stop reconciling and Release it.
func (l *HostLock) Check(ctx context.Context) error {
	if l == nil || l.conn == nil || l.conn.IsClosed() {
		return errors.New("reconcile lock connection closed")
	}
	_, err := l.conn.Exec(ctx, "SELECT 1")
	return err
}

// Release ends the session, which releases the lock. Safe to call twice.
func (l *HostLock) Release() {
	if l == nil || l.conn == nil {
		return
	}
	l.conn.Close(context.Background()) //nolint:errcheck // closing the session releases the lock either way
	l.conn = nil
}
