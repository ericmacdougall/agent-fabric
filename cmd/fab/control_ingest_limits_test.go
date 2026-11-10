package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fabric/internal/storetest"
)

// This exercises the production 30s handler, not a caller-side timeout or a
// shortened test setting. The parent watchdog is exclusively a failing control.
func TestControlIngestServerWall(t *testing.T) {
	const marker = "FABRIC_TEST_INGEST_WALL_CHILD"
	if os.Getenv(marker) == "1" {
		err := controlMain([]string{"ingest", "--source", "harness"})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode(err))
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	input, producer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer producer.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestControlIngestServerWall$")
	child.Env = append(os.Environ(), marker+"=1", storetest.ChildDSNEnv+"=unused-no-database",
		"FABRIC_DSN=postgres://test@"+listener.Addr().String()+"/unused?sslmode=disable")
	child.Stdin = input
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	started := time.Now()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	input.Close() // only the child retains the pipe read end
	if _, err := fmt.Fprintln(producer, `{"schema":"mc.event/1","id":"harness:slow","ts":"2026-11-10T00:00:00Z","kind":"wal"}`); err != nil {
		child.Process.Kill() //nolint:errcheck // own test child only
		child.Wait()         //nolint:errcheck
		t.Fatal(err)
	}
	stop, producerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := producer.Write([]byte(" ")); err != nil {
					return
				}
			}
		}
	}()
	defer func() { close(stop); producer.Close(); <-producerDone }()
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		elapsed := time.Since(started)
		if elapsed < 29*time.Second || elapsed > 33*time.Second || err == nil || child.ProcessState.ExitCode() != 3 || !strings.Contains(stderr.String(), "ingest session exceeded 30s") || stdout.Len() != 0 {
			t.Fatalf("server wall: elapsed=%v exit=%d stdout=%d stderr=%q err=%v", elapsed, child.ProcessState.ExitCode(), stdout.Len(), stderr.String(), err)
		}
	case <-time.After(33 * time.Second):
		child.Process.Kill() //nolint:errcheck // own failing-control child; never a product timeout
		<-done
		t.Fatal("server did not stop slow/open stdin by33s; parent watchdog is a FAILURE")
	}
	listener.SetDeadline(time.Now().Add(20 * time.Millisecond)) //nolint:errcheck
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("ingest attempted database access before input completed")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("canary listener check: %v", err)
	}
}
