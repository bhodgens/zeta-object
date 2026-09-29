// backend_lazy.go — the lazy default backendFor target, kept in its own
// file so the backendFor initialization dependency cannot form a cycle
// (the var's initializer must not (transitively) reference the var).
package main

import "github.com/bhodgens/zeta-object/internal/backend"

// lazyDefaultBackendFor builds the config-driven lookup against
// serverConfig on first use. In production main() installs the lookup
// before the listener opens; this lazy path only serves tests.
func lazyDefaultBackendFor(bucket string) (backend.Backend, error) {
	lookup, err := buildBackendLookup(serverConfig)
	if err != nil {
		return nil, err
	}
	return lookup(bucket)
}
