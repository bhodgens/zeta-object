// versioning_seam.go — the process-level versioning seams the s3
// frontend consumes (s3-versioning tree leaf 03). The wiring layer
// (package main) installs the zfs_versioning mode and the shared zmetad
// DB handle at startup; the zero state (mode "", zdb nil) keeps unit
// tests and the default sidecar behavior unchanged — versionStoreFor
// maps an empty mode to the sidecar store, so OFF/unwired buckets are
// byte-identical to pre-versioning behavior.
//
// The zdb handle follows the InstallMetadataProvider pattern
// (capability_endpoints.go): a package-level hook under hookMu, set
// once by s3_wiring.go from the loaded config's zmetad_db_path.
package s3

import (
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// zmetadDBHook is the installed *metadata.ZmetadDB (the zmetad SQLite
// export database) shared by the ZFS snapshot version store. Guarded by
// hookMu (seam.go). Opened once at startup; nil (missing/legacy DB)
// makes snapshots-mode resolution fail per request with the store's
// honest not-tracked error — never a silent degradation to current data.
var zmetadDBHook *metadata.ZmetadDB

// InstallZmetadDB installs the process-wide zmetad DB handle (exported
// wiring entry). Called once from installS3Seams.
func InstallZmetadDB(db *metadata.ZmetadDB) {
	hookMu.Lock()
	defer hookMu.Unlock()
	zmetadDBHook = db
}

// zmetadDBFor returns the installed zmetad DB handle under a read lock.
func zmetadDBFor() *metadata.ZmetadDB {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return zmetadDBHook
}

// zfsVersioningMode is the server's zfs_versioning config value
// ("snapshots"|"sidecar"|"both"; applies to ZFS-backed buckets only).
// Guarded by hookMu; "" behaves as the sidecar mode everywhere the
// factory consults it.
var zfsVersioningMode string

// InstallZfsVersioningMode installs the configured zfs_versioning mode
// (exported wiring entry). Called once from installS3Seams; the config
// loader has already validated and defaulted the value.
func InstallZfsVersioningMode(mode string) {
	hookMu.Lock()
	defer hookMu.Unlock()
	zfsVersioningMode = mode
}

// zfsVersioningModeFor returns the installed mode under a read lock.
func zfsVersioningModeFor() string {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return zfsVersioningMode
}
