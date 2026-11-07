// Package settle is the single finish-with-retry both launchers use
// (goodqa#15087 AC-7, from goodqa#15082 T6). A worker's end is observed
// once; a transient database error while recording it must not drop it,
// and a real refusal must not be retried into a different answer.
//
// The call it retries must be idempotent by command id, so a retry after a
// lost reply replays the first answer instead of finishing twice.
package settle

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"fabric/internal/store"
)

// The bound: at most MaxTries calls, and no sleep that would end past
// Budget after the first call. The attempt is then left to lease expiry.
const (
	MaxTries = 6
	Budget   = 60 * time.Second
)

// Clock lets tests run the backoff on fake time.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Options tune the backoff; the zero value is the production setting.
type Options struct {
	Clock   Clock
	Backoff time.Duration // first wait; default 1 s
	Max     time.Duration // cap on one wait; default Budget
	// Log, when set, hears about each failed try that will be retried.
	Log func(try int, err error)
}

// Finish runs call until it succeeds, returns an error that is not
// transient, or the bound runs out; it returns the last error.
func Finish(ctx context.Context, o Options, call func(context.Context) error) error {
	clock := o.Clock
	if clock == nil {
		clock = realClock{}
	}
	wait := o.Backoff
	if wait <= 0 {
		wait = time.Second
	}
	max := o.Max
	if max <= 0 || max > Budget {
		max = Budget
	}
	start := clock.Now()
	var err error
	for try := 1; ; try++ {
		if err = call(ctx); err == nil || !Transient(err) {
			return err
		}
		if try >= MaxTries || ctx.Err() != nil {
			return err
		}
		if wait > max {
			wait = max
		}
		if clock.Now().Sub(start)+wait > Budget {
			return err
		}
		if o.Log != nil {
			o.Log(try, err)
		}
		select {
		case <-ctx.Done():
			return err
		case <-clock.After(wait):
		}
		wait *= 2
	}
}

// Transient reports a database error worth one more try: the connection
// went away, the reply was lost, or the transaction lost a serialization
// race. A refusal, a constraint or trigger error, and anything unknown are
// not transient.
func Transient(err error) bool {
	if err == nil || errors.As(err, new(*store.Refusal)) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case pe.Code == "40001", pe.Code == "40P01": // serialization failure, deadlock
			return true
		case len(pe.Code) == 5 && pe.Code[:2] == "08": // connection exception
			return true
		case pe.Code == "57P01", pe.Code == "57P02", pe.Code == "57P03": // shutdown, cannot connect now
			return true
		}
		return false
	}
	if pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return true
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}
