// seam.go — the process-level seams the s3 frontend consumes. The
// wiring layer (package main) installs concrete implementations at
// startup; the zero state serves unit tests. Everything here is
// injection-only: this package never reads package-main state directly.
package s3

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"mini-s3/internal/auth"
	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// serverConfigView is the slice of server configuration the S3 handlers
// need: the custom bucket map (name → path) and the data dir. The wiring
// layer installs it via installServerConfigView.
type serverConfigView struct {
	Buckets map[string]string
	DataDir string
}

// installedServerConfig is the injected config view; nil falls back to an
// empty view (no custom buckets, empty data dir) in tests.
//
// Access is guarded by configViewMu: request goroutines read the view via
// currentServerConfig while tests re-install it concurrently (the pre-move
// serverConfig global had the same unsynchronized pattern).
var (
	configViewMu          sync.RWMutex
	installedServerConfig *serverConfigView
)

// installServerConfigView installs the process configuration view.
// package main calls this once at wiring time (and tests re-install it
// per-case, mirroring the pre-move serverConfig mutation pattern).
func installServerConfigView(cfg serverConfigView) {
	buckets := cfg.Buckets
	if buckets == nil {
		buckets = map[string]string{}
	}
	configViewMu.Lock()
	defer configViewMu.Unlock()
	installedServerConfig = &serverConfigView{Buckets: buckets, DataDir: cfg.DataDir}
}

// currentServerConfig returns the installed view (never nil), re-mirroring
// via the registered sync hook first (test global-mutation support).
func currentServerConfig() *serverConfigView {
	runConfigSyncHook()
	configViewMu.RLock()
	defer configViewMu.RUnlock()
	if installedServerConfig == nil {
		return &serverConfigView{Buckets: map[string]string{}}
	}
	return installedServerConfig
}

// backendLookup is the bucket→Backend seam (former package-main backendFor
// var). The wiring layer installs the config-driven resolver; tests may
// install doubles. Nil falls back to an unavailable lookup, which the
// backendCall helpers surface as InternalError.
//
// Guarded by hookMu (like every runtime-writable seam hook here): request
// goroutines read via installedBackendLookup while wiring/tests re-install.
var backendLookup func(bucket string) (backend.Backend, error)

// backendFor resolves bucket's Backend via the injected lookup.
func backendFor(bucket string) (backend.Backend, error) {
	if installedBackendLookup() == nil {
		return nil, objectmodel.ErrInternalError("backend unavailable")
	}
	return installedBackendLookup()(bucket)
}

// installedBackendLookup returns the current lookup under a read lock.
func installedBackendLookup() func(bucket string) (backend.Backend, error) {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return backendLookup
}

// installBackendLookup installs the bucket→Backend resolver.
func installBackendLookup(fn func(bucket string) (backend.Backend, error)) {
	hookMu.Lock()
	defer hookMu.Unlock()
	backendLookup = fn
}

// hookMu guards every runtime-writable seam hook below (the same
// unsynchronized-global race class configViewMu guards for
// installedServerConfig): request goroutines take the read side, the
// wiring layer/tests take the write side.
var hookMu sync.RWMutex

// lockObjectFn is the per-path write serialization hook (owned by the
// Backend implementation per backend-interface master Contract 4; the
// frontend calls it only for the documented above-seam multipart staging
// and bucket create/delete serialization). The wiring layer installs the
// implementation; the fallback below is a process-local mutex map.
// Guarded by hookMu.
var lockObjectFn func(path string) func()

// objectLocks is the fallback per-path mutex map.
var objectLocks sync.Map // map[string]*sync.Mutex

// defaultLockObject is the pre-move lockObject implementation (used only
// when the wiring layer has not installed a hook).
func defaultLockObject(path string) func() {
	mu, _ := objectLocks.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return func() { mu.(*sync.Mutex).Unlock() }
}

// lockObject serializes writers per path via the injected hook.
func lockObject(path string) func() {
	if fn := installedLockObject(); fn != nil {
		return fn(path)
	}
	return defaultLockObject(path)
}

// installedLockObject returns the current hook under a read lock.
func installedLockObject() func(path string) func() {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return lockObjectFn
}

// writeFileAtomicFn is the atomic-write hook for the above-seam multipart
// staging files. The wiring layer installs the fs implementation.
// Guarded by hookMu.
var writeFileAtomicFn func(path string, data []byte, perm os.FileMode) error

// writeFileAtomic writes data to path atomically via the injected hook.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if fn := installedWriteFileAtomic(); fn != nil {
		return fn(path, data, perm)
	}
	return defaultWriteFileAtomic(path, data, perm)
}

// installedWriteFileAtomic returns the current hook under a read lock.
func installedWriteFileAtomic() func(path string, data []byte, perm os.FileMode) error {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return writeFileAtomicFn
}

// defaultWriteFileAtomic is the pre-move storage.go implementation (temp
// file in the same directory, fsync, rename) — used only when the wiring
// layer has not installed a hook.
func defaultWriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Errorf("generating temp file suffix for %s: %w", path, err)
	}
	tmpPath := path + ".tmp-" + hex.EncodeToString(randBytes[:])

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec // G703: callers pass paths derived from validated object keys (validateObjectKey/validateUploadID).
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil { //nolint:gosec // G703: callers pass validated object-key paths.
		f.Close()
		os.Remove(tmpPath) //nolint:gosec // G703: tmpPath built from validated caller path.
		return fmt.Errorf("writing temp file %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil { //nolint:gosec // G703: callers pass validated object-key paths.
		f.Close()
		os.Remove(tmpPath) //nolint:gosec // G703: tmpPath built from validated caller path.
		return fmt.Errorf("syncing temp file %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil { //nolint:gosec // G703: callers pass validated object-key paths.
		os.Remove(tmpPath) //nolint:gosec // G703: tmpPath built from validated caller path.
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil { //nolint:gosec // G703: callers pass validated object-key paths.
		os.Remove(tmpPath) //nolint:gosec // G703: tmpPath built from validated caller path.
		return fmt.Errorf("renaming %s over %s: %w", tmpPath, path, err)
	}
	return nil
}

// writeFileAtomicJSON marshals v with MarshalIndent("", "  ") then
// writeFileAtomic (pre-move storage.go shape).
func writeFileAtomicJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling JSON for %s: %w", path, err)
	}
	return writeFileAtomic(path, data, perm)
}

// ---------- Exported wiring entry points (package main calls these) ----------

// ServerConfigView is the exported form of serverConfigView for the
// wiring layer.
type ServerConfigView = serverConfigView

// InstallServerConfigView installs the process configuration view.
func InstallServerConfigView(cfg ServerConfigView) { installServerConfigView(cfg) }

// backendIface is exported for the wiring layer's closure typing; it is
// the Backend seam type.
type backendIface = backend.Backend

// InstallBackendLookup installs the bucket→Backend resolver (exported
// wiring entry for installBackendLookup).
func InstallBackendLookup(fn func(bucket string) (backendIface, error)) {
	installBackendLookup(fn)
}

// InstallFSRootResolver installs the bucket→fs-root resolver (exported
// wiring entry).
func InstallFSRootResolver(fn func(bucket string) string) { installFSRootResolver(fn) }

// InstallDefaultCredentialSource installs the process credential source
// (exported wiring entry).
func InstallDefaultCredentialSource(cs auth.CredentialSource) {
	installDefaultCredentialSource(cs)
}

// InstallActionTrigger installs the bucket-actions trigger (exported
// wiring entry).
func InstallActionTrigger(fn func(eventType string, ctx ActionContext)) {
	installActionTrigger(fn)
}

// InstallLockObject installs the per-path write serialization hook
// (exported wiring entry).
func InstallLockObject(fn func(path string) func()) {
	hookMu.Lock()
	defer hookMu.Unlock()
	lockObjectFn = fn
}

// InstallWriteFileAtomic installs the atomic-write hook for the
// above-seam multipart staging (exported wiring entry).
func InstallWriteFileAtomic(fn func(path string, data []byte, perm os.FileMode) error) {
	hookMu.Lock()
	defer hookMu.Unlock()
	writeFileAtomicFn = fn
}

// backendRootResolverHook is the wiring-installed bucket→fs-root function.
// Guarded by hookMu.
var backendRootResolverHook func(bucket string) string

// installFSRootResolver installs the bucket→fs-root resolver used for the
// documented above-seam multipart staging. package main calls this at
// wiring time; when absent, getBucketPath falls back to the config view.
func installFSRootResolver(fn func(bucket string) string) {
	hookMu.Lock()
	defer hookMu.Unlock()
	backendRootResolverHook = fn
}

// fsRootResolver returns the installed resolver under a read lock.
func fsRootResolver() func(bucket string) string {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return backendRootResolverHook
}

// configSyncHook mirrors a wiring-owned mutable config into the view on
// every consult (see SetConfigSyncHook). Guarded by hookMu.
var configSyncHook func()

// SetConfigSyncHook registers a callback invoked on every config view
// consult, letting the wiring layer re-mirror a mutable global.
func SetConfigSyncHook(fn func()) {
	hookMu.Lock()
	defer hookMu.Unlock()
	configSyncHook = fn
}

// runConfigSyncHook invokes the installed sync hook, if any, under a read
// lock (the hook closure itself re-installs the config view, which takes
// configViewMu's write side — never hold both locks around a consult).
func runConfigSyncHook() {
	hookMu.RLock()
	fn := configSyncHook
	hookMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// credentialSyncHook mirrors the wiring-owned credential lookup.
// Guarded by hookMu.
var credentialSyncHook auth.CredentialSource

// SetCredentialSyncHook installs the credential source consulted by the
// default lookup chain.
func SetCredentialSyncHook(fn func(accessKeyID string) (string, bool)) {
	hookMu.Lock()
	defer hookMu.Unlock()
	credentialSyncHook = fnSource(fn)
}

// credentialSourceFor returns the effective process-wide credential
// source under a read lock: the installed source, else the sync-hook
// mirror (tests).
func credentialSourceFor() auth.CredentialSource {
	hookMu.RLock()
	defer hookMu.RUnlock()
	if credentialSyncHook != nil {
		return credentialSyncHook
	}
	return defaultCredentialSourceImpl
}

// fnSource adapts a bare lookup function to the CredentialSource iface.
type fnSource func(accessKeyID string) (string, bool)

func (f fnSource) SecretKey(accessKeyID string) (string, bool) { return f(accessKeyID) }
