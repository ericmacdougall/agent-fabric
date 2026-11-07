package metrics

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ContentType is the Prometheus text exposition content type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// CheckListen accepts host:port where host is a literal loopback IP. Any
// other address (a wildcard such as "" or 0.0.0.0, a routable IP, or a host
// name, which could resolve anywhere) needs allowRemote. The port must be
// numeric.
func CheckListen(addr string, allowRemote bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics: listen address %q: %v", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return fmt.Errorf("metrics: listen port %q is not valid", port)
	}
	if allowRemote {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("metrics: refusing to listen on %q: not a loopback IP (pass --allow-remote to expose metrics without auth)", addr)
	}
	return nil
}

// Handler serves GET and HEAD /metrics only. One collection runs at a time;
// a scrape that arrives while one is running gets 503 rather than another
// database connection. Collection errors are logged, never returned to the
// client.
func Handler(collect func(context.Context) (*Snapshot, error)) http.Handler {
	busy := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			http.Error(w, "a collection is already running", http.StatusServiceUnavailable)
			return
		}
		s, err := collect(r.Context())
		if err != nil {
			log.Printf("metrics: collection failed: %v", err)
			http.Error(w, "collection failed", http.StatusServiceUnavailable)
			return
		}
		var b bytes.Buffer
		if err := s.Write(&b); err != nil {
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		if r.Method == http.MethodHead {
			return
		}
		w.Write(b.Bytes()) //nolint:errcheck // client went away
	})
}

// Server returns an http.Server with conservative timeouts for Handler.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10}
}
