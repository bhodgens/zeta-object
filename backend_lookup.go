// backend_lookup.go — the handler→Backend injection point (backend-interface
// master Contract 5, leaf 03).
//
// buildBackendLookup is the config-driven installer: it resolves each
// bucket's backend type through backend.Registry (explicit per-bucket
// selection or the "fs" default), constructs one Backend per
// (type-name, root) pair, and memoizes instances. Unknown backend type
// names abort startup with an error naming the type and the registered
// names — never a silent fs fallback. Handlers call backendFor and surface
// errors as S3 errors; they must not touch os.* for the seven data-plane
// ops after this leaf.
package main

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"

	"mini-s3/internal/backend"
	"mini-s3/internal/backend/fsbackend"
)

// defaultBackendName is the backend type used when a bucket (or the whole
// config) names none. The name is registered by fsbackend's init().
const defaultBackendName = "fs"

// backendFor is a package-level var so tests can install a recording
// Backend (leaf-02 pin) and the startup path can replace the default with
// the config-driven installer via installBackendLookup. It initializes to
// lazyDefaultBackendFor (backend_lazy.go) so a direct call (handlers,
// tests) never dereferences a nil func value.
//
//nolint:gochecknoglobals // pinned seam shape (master Contract 5)
var backendFor = lazyDefaultBackendFor

// backendForMu guards backendFor's installation slot.
var backendForMu sync.Mutex

// installBackendLookup installs fn as the process-wide bucket→Backend
// resolver (main() calls this BEFORE the listener opens so no request can
// observe a partially built table). installBackendLookup(nil) restores the
// lazy default (tests).
func installBackendLookup(fn func(bucket string) (backend.Backend, error)) {
	backendForMu.Lock()
	defer backendForMu.Unlock()
	if fn == nil {
		backendFor = lazyBackendFor
		return
	}
	backendFor = fn
}

// buildBackendLookup resolves cfg into a memoized bucket→Backend table.
// Root precedence preserves getBucketPath exactly: explicit bucket path >
// named backend's root > dataDir. Two buckets sharing (name, root) share
// one instance; different roots get separate instances.
func buildBackendLookup(cfg ServerConfig) (func(bucket string) (backend.Backend, error), error) {
	var (
		mu       sync.RWMutex
		byKey    = map[string]backend.Backend{} // (name, root) memo
		bucketFn = map[string]func() (backend.Backend, error){}
	)
	constructor := func(name, root string, opts map[string]string) (backend.Backend, error) {
		key := name + "\x00" + root
		mu.RLock()
		b, ok := byKey[key]
		mu.RUnlock()
		if ok {
			return b, nil
		}
		fn, err := backend.Lookup(name)
		if err != nil {
			return nil, err
		}
		b, err = fn(backend.BackendConfig{Type: name, Root: root, Options: opts})
		if err != nil {
			return nil, fmt.Errorf("initializing backend %q (root %q): %w", name, root, err)
		}
		mu.Lock()
		byKey[key] = b
		mu.Unlock()
		return b, nil
	}

	// Register every config-declared bucket (either buckets encoding; the
	// compat view is the Buckets map, selections live in BucketBackends).
	// Construction is EAGER: an unknown backend name fails here, at
	// startup, not on the first request to the affected bucket.
	//
	// Custom-bucket layout pin (bughunt D1 fix): a bucket with an explicit
	// path (root != "") is served by a single-bucket backend whose root IS
	// the configured path — object data, sidecars and the handler-side
	// above-seam staging (multipart, copy meta, sweep, actions) all use
	// <custom>/key, matching getBucketPath. Implicit buckets (root ==
	// dataDir) keep the frozen join(dataDir, bucket) layout.
	for name, path := range cfg.Buckets {
		bucket := name
		backendName := cfg.BucketBackends[bucket]
		if backendName == "" {
			backendName = defaultBackendName
		}
		root := path
		singleBucketOpts := maps.Clone(backendOpts(cfg, backendName))
		if singleBucketOpts == nil {
			singleBucketOpts = map[string]string{}
		}
		if root != "" {
			singleBucketOpts[fsbackend.OptSingleBucketBucket] = bucket
		} else {
			if bc, ok := cfg.Backends[backendName]; ok && bc.Root != "" {
				root = bc.Root
			} else {
				root = cfg.DataDir
			}
		}
		if _, err := constructor(backendName, root, singleBucketOpts); err != nil {
			return nil, err
		}
		rootForKey := root
		optsForKey := singleBucketOpts
		bucketFn[bucket] = func() (backend.Backend, error) {
			return constructor(backendName, rootForKey, optsForKey)
		}
	}

	defaultRoot := cfg.DataDir
	if def, ok := cfg.Backends[defaultBackendName]; ok && def.Root != "" {
		defaultRoot = def.BackendDefaultRoot(defaultRoot)
	}
	defaultOpts := backendOpts(cfg, defaultBackendName)

	return func(bucket string) (backend.Backend, error) {
		if fn, ok := bucketFn[bucket]; ok {
			return fn()
		}
		return constructor(defaultBackendName, defaultRoot, defaultOpts)
	}, nil
}

// BackendDefaultRoot lets a named backend entry override the default root;
// it exists so the default-root resolution stays one expression.
func (b BackendCfg) BackendDefaultRoot(fallback string) string {
	if b.Root != "" {
		return b.Root
	}
	return fallback
}

// backendOpts returns the Options of cfg's named backend entry (nil when
// the type is not declared).
func backendOpts(cfg ServerConfig, name string) map[string]string {
	if bc, ok := cfg.Backends[name]; ok {
		return bc.Options
	}
	return nil
}

// lazyBackendFor builds the config-driven lookup against serverConfig on
// first use. In production main() installs the lookup before the listener
// opens; this lazy path only serves tests (and would, if hit, still fail
// loudly on an unknown backend name).
func lazyBackendFor(bucket string) (backend.Backend, error) {
	lookup, err := buildBackendLookup(serverConfig)
	if err != nil {
		return nil, err
	}
	installBackendLookup(lookup)
	return lookup(bucket)
}

// initBackendLookup is the startup entry point main() calls before the
// listener opens: it builds the table eagerly so an unknown backend name
// aborts startup with a clear error, and logs the registered type names.
func initBackendLookup() error {
	lookup, err := buildBackendLookup(serverConfig)
	if err != nil {
		return err
	}
	installBackendLookup(lookup)
	log.Printf("Registered backends: %s", strings.Join(backend.Names(), ", "))
	return nil
}
