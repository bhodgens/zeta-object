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
	"log"
	"os"
	"path/filepath"
	"strconv"
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
		Buckets: serverConfig.Buckets,
		DataDir: serverConfig.DataDir,
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
	// built-in zfs-events provider and installs the per-bucket resolver
	// hook the ?events endpoints consult.
	//
	// The resolver keeps the shim's per-request PROBE semantics: the
	// provider is only returned for buckets whose path sits on a ZFS
	// dataset with the events feature on, so registration alone never
	// makes a non-ZFS bucket claim availability (unavailable probes
	// return nil → contracted 503).
	metadata.Register(metadata.NewZFSEventsProvider())
	s3.InstallMetadataProvider(func(bucketPath string) metadata.MetadataProvider {
		p := metadata.Lookup("zfs-events")
		if p == nil {
			return nil
		}
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

// sweepExpiredUploads is the frontend's above-seam sweep entry (it lives
// with the multipart staging code); package main keeps the ticker.
var _ = log.Println
var _ = os.Stat
var _ = strconv.Quote
var _ = auth.Identity{}

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
