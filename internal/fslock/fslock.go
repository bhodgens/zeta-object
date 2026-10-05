// Package fslock owns THE process-wide per-path write lock table.
//
// It was extracted verbatim from internal/frontend/s3/seam.go (the installed
// hook plus the sync.Map fallback) so that the S3 frontend and the shared
// bucket manager serialize on ONE table: an S3 create/delete and a management
// create/delete of the same bucket can never interleave.
//
// The wiring layer (package main) installs the concrete implementation via
// Install at startup; the zero state is the process-local mutex map, which
// serves unit tests (mirroring the pre-move seam.go fallback). The s3
// frontend keeps its own lockObject entry point (see seam.go) but it now
// delegates here, and its InstallLockObject delegates to Install.
package fslock

import "sync"

// Locker is the lock-table surface the shared bucket manager consumes: Lock
// takes the per-path lock and returns its release function.
type Locker interface {
	Lock(path string) func()
}

// hookMu guards the runtime-writable wiring hook: request goroutines read it
// while the wiring layer/tests install it.
var hookMu sync.RWMutex

// lockHook is the wiring-installed per-path lock implementation; nil falls
// back to the process-local mutex map below. Guarded by hookMu.
var lockHook func(path string) func()

// defaultLocks is the fallback per-path mutex map (the pre-move
// defaultLockObject table).
var defaultLocks sync.Map // map[string]*sync.Mutex

// defaultLock is the pre-move lockObject implementation, used only when the
// wiring layer has not installed a hook.
func defaultLock(path string) func() {
	mu, _ := defaultLocks.LoadOrStore(path, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// installedHook returns the current hook under a read lock.
func installedHook() func(path string) func() {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return lockHook
}

// Lock serializes writers per path via the injected hook (or the fallback
// mutex map). It returns the release function.
func Lock(path string) func() {
	if fn := installedHook(); fn != nil {
		return fn(path)
	}
	return defaultLock(path)
}

// Install installs the per-path write serialization hook. The wiring layer
// (package main) calls this once at startup; tests re-install per case.
func Install(fn func(path string) func()) {
	hookMu.Lock()
	defer hookMu.Unlock()
	lockHook = fn
}

// Default is a Locker backed by the package Lock function: the value the
// bucket manager's Env.Locks field carries so both callers share this table.
var Default Locker = defaultLocker{}

type defaultLocker struct{}

func (defaultLocker) Lock(path string) func() { return Lock(path) }
