// backend_lookup.go — the handler→Backend injection point (backend-interface
// master Contract 5, leaf 02).
//
// backendFor resolves the Backend serving a bucket. Leaf 02 installs the
// default: every bucket is served by one fsbackend.FS rooted at dataDir;
// config-declared custom buckets get their own FS rooted at the configured
// path (preserving getBucketPath semantics exactly). Leaf 03 replaces the
// default installer with registry/config-driven construction. Handlers call
// backendFor and surface errors as S3 errors; they must not touch os.* for
// the seven data-plane ops after this leaf.
package main

import (
	"sync"

	"mini-s3/internal/backend"
	"mini-s3/internal/backend/fsbackend"
)

// backendFor is a package-level var so tests can install a recording
// Backend (leaf-02 pin) and leaf 03 can replace the installer. When nil,
// defaultBackendFor applies.
//
//nolint:gochecknoglobals // pinned seam shape (master Contract 5)
var backendFor = defaultBackendFor
var (
	backendMu     sync.Mutex
	defaultFS     *fsbackend.FS
	defaultFSRoot string
	customFS      = map[string]*fsbackend.FS{} // custom bucket path → FS
)

// defaultBackendFor resolves bucket against serverConfig: a custom bucket
// returns an FS rooted at its configured path; any other bucket is served
// by the memoized FS rooted at dataDir. A config change of DataDir (tests,
// leaf-03 reload) rebuilds the memoized instance. Custom paths are memoized
// per path: the config map is fixed after loadConfig.
func defaultBackendFor(bucket string) (backend.Backend, error) {
	if customPath, ok := serverConfig.Buckets[bucket]; ok {
		backendMu.Lock()
		defer backendMu.Unlock()
		if f, exists := customFS[customPath]; exists {
			return f, nil
		}
		f, err := fsbackend.New(customPath)
		if err != nil {
			return nil, err
		}
		customFS[customPath] = f
		return f, nil
	}

	backendMu.Lock()
	defer backendMu.Unlock()
	if defaultFS == nil || defaultFSRoot != serverConfig.DataDir {
		f, err := fsbackend.New(serverConfig.DataDir)
		if err != nil {
			return nil, err
		}
		defaultFS = f
		defaultFSRoot = serverConfig.DataDir
	}
	return defaultFS, nil
}
