// Package backend — registry.go: the name → constructor registry and
// BackendConfig (frozen shapes per backend-interface-2026-09 master
// Contract 2). Leaf 02 owns the BackendConfig type + Register sink so
// fsbackend's init() registration hook can exist; leaf 03 adds
// Lookup/Names, thread-safety hardening, and the config-driven installer.
package backend

import "sync"

// BackendConfig carries the construction parameters for a registered
// backend type. Root is the storage root (dataDir or a custom bucket path);
// Options carries backend-specific string settings (populated by leaf 03's
// config plumbing).
type BackendConfig struct {
	Type    string
	Root    string
	Options map[string]string
}

// registry is the name → constructor table, populated by each backend
// package's init() and consumed by Lookup (leaf 03).
var (
	registryMu sync.RWMutex
	registry   = map[string]func(cfg BackendConfig) (Backend, error){}
)

// Register installs a constructor under name. Panics on duplicate
// registration (an init-time programming error, pinned by leaf 03's spec).
func Register(name string, fn func(cfg BackendConfig) (Backend, error)) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("backend: duplicate registration for " + name)
	}
	registry[name] = fn
}

// Lookup returns the constructor registered under name, or
// ErrUnknownBackend (never a silent fs fallback).
func Lookup(name string) (func(cfg BackendConfig) (Backend, error), error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	fn, ok := registry[name]
	if !ok {
		return nil, ErrUnknownBackend
	}
	return fn, nil
}

// Names returns the sorted registered backend type names (leaf 03's
// error-message convention).
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
