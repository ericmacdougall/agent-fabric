// Package dispatch is fabd's provider registry (goodqa#15373 D1). A
// Dispatcher plans one launch (argv, env and the bytes of a stdin file) and
// settles the finished stream; it never starts a process. The host
// Launcher's StartIO is the only process start, so a registry entry runs
// inside the same host confinement as today's argv path.
package dispatch

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"sync"

	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// Input is what the scheduler hands a Dispatcher for one attempt: the
// operator's launch settings and the projection prompt.
type Input struct {
	Launch claude.Launch
	Prompt string
}

// Launch is one planned start. StdinBytes become the run dir's prompt.jsonl,
// which the scheduler opens read-only as the worker's stdin.
type Launch struct {
	Argv       []string
	Env        []string
	StdinBytes []byte
}

// Settlement is the decision a finished stream yields. None means no result
// event: the attempt is left to lease expiry (UNKNOWN), never relaunched.
// Usage holds the usage.json bytes, or nil when the stream was unreadable.
type Settlement struct {
	None    bool
	Outcome string
	WaitKey string
	Reason  string
	Usage   []byte
}

// Dispatcher plans and settles one provider's runs.
type Dispatcher interface {
	Name() string
	Plan(g host.Grant, in Input) (Launch, error)
	Settle(stream io.Reader, exit int) (Settlement, error)
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Registry maps provider names to dispatchers, with harness.Registry's name
// rule and duplicate refusal.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Dispatcher
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Dispatcher{}} }

// Register adds d. It refuses a nil dispatcher, a malformed or duplicate
// name, and a dispatcher whose name changes between calls.
func (r *Registry) Register(d Dispatcher) error {
	if d == nil {
		return fmt.Errorf("dispatch: nil dispatcher")
	}
	if v := reflect.ValueOf(d); v.Kind() == reflect.Ptr && v.IsNil() {
		return fmt.Errorf("dispatch: nil dispatcher (typed nil %T)", d)
	}
	n := d.Name()
	if !nameRE.MatchString(n) {
		return fmt.Errorf("dispatch: dispatcher name %q must match %s", n, nameRE)
	}
	if d.Name() != n {
		return fmt.Errorf("dispatch: dispatcher %q changes its name", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[n]; ok {
		return fmt.Errorf("dispatch: dispatcher %q already registered", n)
	}
	r.m[n] = d
	return nil
}

// Get returns the dispatcher for name.
func (r *Registry) Get(name string) (Dispatcher, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.m[name]
	return d, ok
}

// Names lists registered names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
