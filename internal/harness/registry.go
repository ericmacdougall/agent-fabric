package harness

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Registry maps harness names to adapters. It replaces the closed provider
// enum: adding a harness is registering an adapter, with no change to the
// reducer or the database.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Adapter{}} }

// Register adds a. It refuses a nil adapter, a malformed or duplicate name,
// and an adapter whose Name changes between calls (a registry key must be
// stable).
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("harness: nil adapter")
	}
	n := a.Name()
	if !nameRE.MatchString(n) {
		return fmt.Errorf("harness: adapter name %q must match %s", n, nameRE)
	}
	if a.Name() != n {
		return fmt.Errorf("harness: adapter %q changes its name", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[n]; ok {
		return fmt.Errorf("harness: adapter %q already registered", n)
	}
	r.m[n] = a
	return nil
}

// Get returns the adapter for name.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[name]
	return a, ok
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
