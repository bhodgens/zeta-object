package frontend

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds Frontends keyed by Name(). Safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	frontends map[string]Frontend
}

func NewRegistry() *Registry {
	return &Registry{frontends: make(map[string]Frontend)}
}

func (r *Registry) Register(f Frontend) error {
	if f == nil {
		return fmt.Errorf("frontend: cannot register nil frontend")
	}
	name := f.Name()
	if name == "" {
		return fmt.Errorf("frontend: cannot register frontend with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.frontends[name]; exists {
		return fmt.Errorf("frontend: %q already registered", name)
	}
	r.frontends[name] = f
	return nil
}

func (r *Registry) Lookup(name string) (Frontend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.frontends[name]
	return f, ok
}

// All returns registered frontends in deterministic (name-sorted) order.
func (r *Registry) All() []Frontend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.frontends))
	for name := range r.frontends {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Frontend, 0, len(names))
	for _, name := range names {
		out = append(out, r.frontends[name])
	}
	return out
}
