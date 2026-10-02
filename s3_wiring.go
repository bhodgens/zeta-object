// s3_wiring.go — the process seams package main installs for the s3
// frontend (frontend-interface leaf 02). Everything the extracted
// frontend needs from the process lives behind these adapters:
//
//   - serverConfigView: the server configuration slice (custom bucket
//     map + dataDir)
//   - backendFor: the config-driven bucket→Backend resolver built by
//     backend_lookup.go (installed into the frontend's seam)
//   - getBucketPath: the bucket→fs-root resolver for the documented
//     above-seam multipart staging (fs layout is the backend's frozen
//     contract)
//   - credentials: serverCredentials wrapped in auth.CredentialSource
//   - actions: the bucket-actions trigger adapter
//
// package main retains startMultipartExpirySweeper and storage helpers
// (storage.go) — the sweeper stays main-side (it sweeps buckets, not
// requests), and the atomic-write helpers are exported to the frontend
// through s3 seam hooks so the above-seam multipart staging keeps its
// exact fs behavior.
package main

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// mainCredentialSource adapts serverCredentials to auth.CredentialSource.
// v1 has exactly one pair; the auth GH issue replaces this.
type mainCredentialSource struct{}

func (mainCredentialSource) SecretKey(accessKeyID string) (string, bool) {
	if accessKeyID == serverCredentials.AccessKeyID {
		return serverCredentials.SecretAccessKey, true
	}
	return "", false
}

// installS3Seams wires every process-level seam the s3 frontend consumes.
// Called from main() before the listener opens (and from tests that drive
// the frontend against real fs storage).
func installS3Seams() {
	// Configuration view.
	s3.InstallServerConfigView(s3.ServerConfigView{
		Buckets:    serverConfig.Buckets,
		DataDir:    serverConfig.DataDir,
		AuditReads: serverConfig.BucketAuditReads,
	})

	// Data plane: install the SAME backendFor resolver the handlers used
	// pre-move (config-driven lookup from backend_lookup.go).
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) {
		return backendFor(bucket)
	})

	// Above-seam multipart staging root: getBucketPath semantics exactly
	// (custom path wins, else dataDir/bucket).
	s3.InstallFSRootResolver(func(bucket string) string {
		return getBucketPath(bucket)
	})

	// Credentials.
	s3.InstallDefaultCredentialSource(mainCredentialSource{})

	// Multi-identity registry (pluggable-authentication tree leaves 01/02):
	// built from the env pair + config identities (validated fail-loud in
	// main(); a nil here means validation already aborted startup — the
	// frontend falls back to the legacy single-pair source above).
	//
	// Design-leaf 08 (key rotation/revocation): main() wraps the startup
	// registry in an auth.ReloadableRegistry BEFORE wiring, and that
	// wrapper is what is installed here (recommended Open Decision 1 —
	// wrap at construction). The SIGHUP handler swaps the wrapper's INNER
	// registry, so this hook observes every swap with no seam change.
	if identityRegistry != nil {
		s3.InstallIdentityRegistry(identityRegistry)
	}

	// Design-leaf 08: the reloadable wrapper ALSO serves as the legacy
	// env-fallback CredentialSource, so the fallback signing-secret path
	// rotates on SIGHUP too (instead of pinning the startup env pair
	// forever via a static mainCredentialSource). ReloadableRegistry
	// satisfies auth.CredentialSource (compile-asserted in internal/auth).
	s3.InstallDefaultCredentialSource(identityRegistry)

	// Zero-auth dev mode (leaf 05): auth.mode "none" installs the
	// DevAuthenticator as the process authenticator source — every request
	// authenticates as the loud anonymous wildcard identity. Opt-in only;
	// config validation errors abort startup before this line.
	if serverConfig.Auth.Mode == authModeNone {
		dev := auth.NewDevAuthenticator(nil)
		dev.Banner()
		s3.InstallDevAuthenticator(dev)
	}

	// Bucket-actions trigger (adapter converts the identical-shaped
	// context structs in one place).
	s3.InstallActionTrigger(func(eventType string, ctx s3.ActionContext) {
		triggerActions(eventType, mainActionContext(ctx))
	})

	// Per-path write serialization + atomic writes for the above-seam
	// multipart staging (storage.go remains the implementation owner).
	s3.InstallLockObject(lockObject)
	s3.InstallWriteFileAtomic(writeFileAtomic)

	// Metadata provider registration + per-bucket resolver (bughunt C1).
	// The blank import in main.go guarantees the metadata package (and its
	// provider) is linked into the production binary; this registers the
	// built-in zfs-events provider (zmetad DB-backed since
	// zmetad-provider-2026-09 leaf 04) and installs the per-bucket
	// resolver hook the ?events endpoints consult.
	//
	// The resolver keeps the shim's per-request PROBE semantics: the
	// provider is only returned for buckets whose path sits on a ZFS
	// dataset that zmetad tracks and has polled, so registration alone
	// never makes a non-ZFS bucket claim availability (unavailable probes
	// return nil → contracted 503).
	//
	// Config flow mirrors the dataDir seam above: installS3Seams reads
	// the loaded serverConfig global directly. Defaults land at config
	// load (config.go), so the provider always receives concrete values.
	registered := metadata.NewZmetadEventsProvider(serverConfig.ZmetadDBPath)
	metadata.SetZmetadBinary(registered, serverConfig.ZmetadBinary)
	metadata.Register(registered)
	s3.InstallMetadataProvider(func(bucketPath string) metadata.MetadataProvider {
		// A FRESH provider instance per bucket (bughunt M1): the
		// registry singleton is shared across buckets, and its
		// per-instance LastDetail would cross-attribute dataset/
		// recordsLost between concurrent ?events on different buckets.
		// Construction is cheap (no I/O until Probe opens the DB);
		// Probe keeps the per-request availability semantics.
		p := metadata.NewZmetadEventsProvider(serverConfig.ZmetadDBPath)
		metadata.SetZmetadBinary(p, serverConfig.ZmetadBinary)
		res, err := p.Probe(context.Background(), bucketPath)
		if err != nil || !res.Available {
			return nil
		}
		return p
	})

	// The multipart expiry sweep pass runs in the frontend; package main
	// keeps the hourly ticker (runMultipartSweepPass drives it).
	setSweepEntries(s3.SweepAllBucketsOnce)
}

// mainActionContext converts the frontend's ActionContext to the
// package-main actions.go struct (identical fields; one adapter site).
func mainActionContext(ctx s3.ActionContext) ActionContext {
	return ActionContext{
		FilePath:     ctx.FilePath,
		MetadataPath: ctx.MetadataPath,
		BucketName:   ctx.BucketName,
		BucketPath:   ctx.BucketPath,
		ObjectKey:    ctx.ObjectKey,
		ContentType:  ctx.ContentType,
		ETag:         ctx.ETag,
		Size:         ctx.Size,
	}
}

// getBucketPath returns the filesystem path for a bucket (custom mapping
// first, then dataDir) — unchanged pre-move semantics; the s3 frontend
// reaches the same math through its injected fs-root resolver.
func getBucketPath(bucketName string) string {
	if customPath, ok := serverConfig.Buckets[bucketName]; ok {
		return customPath
	}
	return filepath.Join(serverConfig.DataDir, bucketName)
}

// multipartSweepEntry is installed by the s3 frontend (installS3Seams
// below); nil = no staging sweep installed (unit tests). Guarded by
// sweepMu: the sweeper goroutine reads it while wiring/tests install.
var (
	sweepMu             sync.RWMutex
	multipartSweepEntry func() int
)

// sweepAllBucketsOnce is the test-visible alias of the frontend's sweep
// pass (multipart_sweeper_test.go drives it directly, as pre-move).
// Guarded by sweepMu.
var sweepAllBucketsOnce func() int

// setSweepEntries installs both sweep entries atomically.
func setSweepEntries(entry func() int) {
	sweepMu.Lock()
	defer sweepMu.Unlock()
	multipartSweepEntry = entry
	sweepAllBucketsOnce = entry
}

// runMultipartSweepPass performs one expiry sweep over every discovered
// bucket (dataDir buckets + configured custom buckets) through the
// frontend's sweep entry.
func runMultipartSweepPass() int {
	sweepMu.RLock()
	entry := multipartSweepEntry
	sweepMu.RUnlock()
	if entry == nil {
		return 0
	}
	return entry()
}

// sweepInterval and the sweeper ticker stay package-main (they were here
// pre-move); the sweep PASS runs in the frontend over the same bucket
// roots it stages parts under.
var sweepInterval = time.Hour

// startMultipartExpirySweeper runs the hourly expiry sweep (former
// multipart_handlers.go ticker; the pass body lives in the frontend).
func startMultipartExpirySweeper() {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			runMultipartSweepPass()
		}
	}()
}
