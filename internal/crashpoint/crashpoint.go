// Package crashpoint marks every durable write on the M1 launch, hold,
// FINISH, settle, release and recovery paths (goodqa#15966), so a test can
// kill a real process at each one and check what a restart recovers.
//
// A point is armed only when both hold:
//   - FABRIC_CRASHPOINT is "<name>:<before|after>" or
//     "<name>:<before|after>:<n>" (the n-th time the point is reached,
//     from 1; default 1) at process start, and
//   - the process is a Go test binary (testing.Testing()).
//
// Otherwise Hit, After and Around only call through: production binaries
// read the variable but never act on it, in either build.
package crashpoint

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Env names the variable that arms one point in a test child.
const Env = "FABRIC_CRASHPOINT"

// ExitCode is the status of a process killed at a point (128+SIGKILL).
const ExitCode = 137

// Marker prefixes the one stderr line a child writes before it exits, so
// the parent can tell the point it died at from any other exit.
const Marker = "CRASHPOINT "

// Trace is the FABRIC_CRASHPOINT value that kills nothing and writes one
// TraceMarker line, "<name>:<pos>:<n>", each time a point is reached, so a
// test learns a scenario's points and how often each occurs.
const (
	Trace       = "trace"
	TraceMarker = "CRASHPOINT-TRACE "
)

type arm struct {
	name, pos string
	n         int
}

var (
	armed = parse(os.Getenv(Env), testing.Testing())
	mu    sync.Mutex
	seen  = map[string]int{}
)

func parse(v string, test bool) arm {
	if v == "" || !test {
		return arm{}
	}
	if v == Trace {
		return arm{name: Trace, pos: Trace}
	}
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || (parts[1] != "before" && parts[1] != "after") {
		return arm{}
	}
	a := arm{name: parts[0], pos: parts[1], n: 1}
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 {
			return arm{}
		}
		a.n = n
	}
	return a
}

func fire(name, pos string) {
	if armed.name == "" || (armed.name != Trace && (name != armed.name || pos != armed.pos)) {
		return
	}
	mu.Lock()
	seen[name+":"+pos]++
	n := seen[name+":"+pos]
	mu.Unlock()
	if armed.name == Trace {
		fmt.Fprintf(os.Stderr, "%s%s:%s:%d\n", TraceMarker, name, pos, n)
		return
	}
	if n != armed.n {
		return
	}
	fmt.Fprintf(os.Stderr, "%s%s:%s:%d\n", Marker, name, pos, n)
	os.Exit(ExitCode)
}

// Hit is the point just before the durable write it names.
func Hit(name string) { fire(name, "before") }

// After is the point just after the write returned, success or not.
func After(name string) { fire(name, "after") }

// Around runs write between Hit(name) and After(name).
func Around(name string, write func() error) error {
	Hit(name)
	err := write()
	After(name)
	return err
}
