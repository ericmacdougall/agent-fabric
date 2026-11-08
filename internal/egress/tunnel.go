package egress

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var errTunnelLimit = errors.New("egress: tunnel limit reached")

type tunnel struct {
	client net.Conn
	target net.Conn
	idle   time.Duration
	total  time.Time

	lastActivity atomic.Int64
	mu           sync.Mutex
	remaining    int64
	closeOnce    sync.Once
	stop         chan struct{}
}

type relayResult struct {
	dst net.Conn
	err error
}

func (m *Mediator) runTunnel(client, target net.Conn, clientReader *bufio.Reader, total time.Time) {
	t := &tunnel{
		client: client, target: target,
		idle: m.limits.IdleTimeout, total: total,
		remaining: m.limits.MaxBytes,
		stop:      make(chan struct{}),
	}
	t.lastActivity.Store(time.Now().UnixNano())
	results := make(chan relayResult, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		t.relay(results, target, clientReader)
	}()
	go func() {
		defer wg.Done()
		t.relay(results, client, target)
	}()
	go func() {
		defer wg.Done()
		t.watch(results)
	}()

	first := <-results
	if first.err != nil {
		t.close()
	} else {
		closeWrite(first.dst)
	}
	<-results
	t.close()
	close(t.stop)
	wg.Wait()
}

func (t *tunnel) relay(results chan<- relayResult, dst net.Conn, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		t.mu.Lock()
		remaining := t.remaining
		t.mu.Unlock()
		if remaining <= 0 {
			results <- relayResult{dst: dst, err: errTunnelLimit}
			return
		}
		readBuf := buf
		if int64(len(readBuf)) > remaining {
			readBuf = readBuf[:remaining]
		}
		n, readErr := src.Read(readBuf)
		allowed := t.consume(n)
		if allowed > 0 {
			t.lastActivity.Store(time.Now().UnixNano())
			if _, err := dst.Write(buf[:allowed]); err != nil {
				results <- relayResult{dst: dst, err: err}
				return
			}
		}
		if allowed < n {
			results <- relayResult{dst: dst, err: errTunnelLimit}
			return
		}
		if readErr != nil {
			results <- relayResult{dst: dst, err: normalizeRelayError(readErr)}
			return
		}
	}
}

func normalizeRelayError(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// consume bounds bytes relayed in both directions combined. Concurrent reads
// may be truncated, but writes can never exceed the configured budget.
func (t *tunnel) consume(n int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if int64(n) > t.remaining {
		n = int(t.remaining)
	}
	t.remaining -= int64(n)
	return n
}

func (t *tunnel) watch(results chan<- relayResult) {
	interval := t.idle / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-ticker.C:
			last := time.Unix(0, t.lastActivity.Load())
			idleDeadline := last.Add(t.idle)
			deadline := minTime(idleDeadline, t.total)
			if !deadline.After(now) {
				t.close()
				results <- relayResult{err: errTunnelLimit}
				return
			}
			t.client.SetDeadline(deadline)
			t.target.SetDeadline(deadline)
		}
	}
}

func (t *tunnel) close() {
	t.closeOnce.Do(func() {
		t.client.Close()
		t.target.Close()
	})
}

func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
	}
}
