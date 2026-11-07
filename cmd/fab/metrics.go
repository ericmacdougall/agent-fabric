package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"fabric/internal/metrics"
	"fabric/internal/native"
	"fabric/internal/store"
)

// metricsOptions fills what the collector cannot read from the database:
// the build, this store's dispatch limit and the fixed spend ceilings.
func metricsOptions(st *store.Store, timeout time.Duration) metrics.Options {
	return metrics.Options{Timeout: timeout, DispatchLimit: cap(st.DispatchSlots()), Version: buildVersion(),
		Caps: []metrics.Cap{{Scope: "month", Micros: native.MonthCapMicros}, {Scope: "job", Micros: native.JobCapMicros},
			{Scope: "live_test", Micros: native.LiveTestCapMicros}}}
}

// buildVersion is the module version or the VCS revision the binary was
// built from.
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return bi.Main.Version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// fab metrics: one collection, printed in the Prometheus text format.
func metricsMain(args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	timeout := fs.Duration("timeout", metrics.DefaultTimeout, "statement and lock timeout for the collection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		s, err := metrics.Collect(ctx, st.Pool(), metricsOptions(st, *timeout))
		if err != nil {
			return err
		}
		return s.Write(os.Stdout)
	})
}

// fab serve-metrics: GET /metrics over HTTP, loopback only unless
// --allow-remote. There is no auth and no write route.
func serveMetricsMain(args []string) error {
	fs := flag.NewFlagSet("serve-metrics", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9464", "host:port to listen on (a loopback IP unless --allow-remote)")
	allowRemote := fs.Bool("allow-remote", false, "allow a non-loopback listen address (metrics are served without auth)")
	timeout := fs.Duration("timeout", metrics.DefaultTimeout, "statement and lock timeout per collection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := metrics.CheckListen(*listen, *allowRemote); err != nil {
		return err
	}
	dsn := os.Getenv("FABRIC_DSN")
	if dsn == "" {
		return errors.New("FABRIC_DSN is not set")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	st, err := store.Open(openCtx, dsn)
	cancel()
	if err != nil {
		return err
	}
	defer st.Close()
	o := metricsOptions(st, *timeout)
	srv := metrics.Server(*listen, metrics.Handler(*listen, func(c context.Context) (*metrics.Snapshot, error) {
		return metrics.Collect(c, st.Pool(), o)
	}))
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("fab serve-metrics: listening on http://%s/metrics", *listen)
	select {
	case err := <-errc:
		return fmt.Errorf("serve-metrics: %w", err)
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
