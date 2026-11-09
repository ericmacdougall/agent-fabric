package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"fabric/internal/store"
)

// goodqa#15579. withStoreService runs a long-lived service: no deadline on the
// run, and SIGTERM (or SIGINT) ends it cleanly with a nil error.
func TestWithStoreServiceHasNoDeadlineAndStopsOnSIGTERM(t *testing.T) {
	dsn := os.Getenv("FABRIC_TEST_DSN")
	if dsn == "" {
		t.Skip("FABRIC_TEST_DSN is not set")
	}
	t.Setenv("FABRIC_DSN", dsn)
	started := make(chan struct{})
	done := make(chan error, 1)
	var hadDeadline bool
	go func() {
		done <- withStoreService(func(ctx context.Context, st *store.Store) error {
			_, hadDeadline = ctx.Deadline()
			close(started)
			<-ctx.Done()
			return ctx.Err() // what Scheduler.Run returns when it is stopped
		})
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the service body never started")
	}
	if hadDeadline {
		t.Fatal("the service context has a deadline: the daemon would end after it (goodqa#15579)")
	}
	select {
	case err := <-done:
		t.Fatalf("the service ended by itself: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM gave %v, want a clean nil shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service did not stop on SIGTERM")
	}
}

// A real failure inside the service is still an error, signal or not.
func TestWithStoreServiceReturnsRealErrors(t *testing.T) {
	dsn := os.Getenv("FABRIC_TEST_DSN")
	if dsn == "" {
		t.Skip("FABRIC_TEST_DSN is not set")
	}
	t.Setenv("FABRIC_DSN", dsn)
	boom := errors.New("store outage")
	err := withStoreService(func(ctx context.Context, st *store.Store) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the service's own error", err)
	}
}
