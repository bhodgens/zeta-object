// zfssurface.go — quic-h3-2026-10 leaf 06: the ZFS enrichment READ
// surfaces over the webdav path. A client that speaks only WebDAV reads
// the SAME event history and version data an S3 client gets from
// ?events / ?events&versions / ?versions — identical data, identical
// wire forms (universality rule, master decision 7). Everything here
// delegates to the s3 package's bridge (zfssurface_bridge.go): the JSON
// envelopes, the derived XML listing, and the provider resolution are
// ONE implementation — never duplicated across frontends.
//
// Dispatch contract (pinned by zfssurface_test.go):
//   - ?events on ANY GET (collection or file) resolves to the events
//     surface; ?events&versions on a collection resolves to the events
//     extension (the derived XML listing) — the same precedence the s3
//     bucket dispatch pins (events above the plain versions check).
//   - ?versions on a FILE GET resolves to that file's JSON version
//     listing. ?versions on a collection GET stays the plain path (the
//     param is ignored there — the pre-leaf behavior, byte-unchanged).
//   - Unknown query params keep today's behavior exactly (ignored).
//   - Provider-unavailable answers the SAME honest 503 body the s3
//     endpoint answers (shared writeNoProviderError).
//
// Auth: these are ordinary GETs in the dispatch table — the webdav
// frontend's existing authenticator and grant gates run before any of
// this code (serveHTTP pipeline). No new auth surface.
//
// h3: no code — the h3 frontend wraps this handler (leaf 02), so every
// surface here is served over QUIC automatically.
package webdav

import (
	"net/http"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// zfsSurfaceDispatch resolves the ZFS enrichment queries on a GET/HEAD
// BEFORE any resource resolution: ?events on any path, ?versions on a
// file path. Returns true when it answered the request. The caller runs
// this right after authorization (so auth still gates everything) and
// before handleGET's resource-kind switch — a query on a collection must
// never fall into the collection-GET path.
func (f *Frontend) zfsSurfaceDispatch(w http.ResponseWriter, r *http.Request, res resource) bool {
	if r.Method != "GET" && r.Method != "HEAD" {
		return false
	}
	query := r.URL.Query()
	// Mode A's root ("/") is the synthetic bucket list — no bucket
	// semantics, no events surface. Mode B's "/" IS the configured
	// bucket's root collection (parseResource sets isRoot there too),
	// so only a root with NO bucket skips.
	if res.isRoot && res.bucket == "" {
		return false
	}
	bucketPath := f.bucketPath(res.bucket)
	if bucketPath == "" {
		return false // unwired seam (unit-test shape): plain behavior
	}
	// ?events on ANY GET (collection or file). The combined
	// ?events&versions resolves INSIDE the bridge to the events
	// extension, matching the s3 dispatch precedence.
	//
	// BUGHUNT L2: a NESTED COLLECTION also carries a non-empty key
	// ("photos/report/" parses to key "photos/report", isCollection
	// true), so `res.key == ""` is NOT the collection test — it only
	// selects the bucket-level (root / bucket-root) form. Routing a
	// nested collection into the key-scoped surface asked the provider
	// for the events of the key "photos/report", and the zmetad row
	// matcher's PARTIAL-row rule (bare-name equality, or the queried key
	// ending in "/"+bare) can then answer with a COMPLETELY UNRELATED
	// object's events — a row recorded under the bare name "report"
	// matched this collection's last segment. S3 has no collection
	// concept, so its surface cannot produce that answer at all.
	//
	// The fix classifies the resource FIRST: a collection gets its own
	// honest surface (empty history — a collection names no object, and
	// asking the provider for it could match an unrelated object's rows),
	// and only a real FILE reaches the key-scoped surface. A collection is
	// only ever reached through the trailing-slash form (parseResource
	// sets isCollection only for a slash-terminated path), so no FILE
	// answer changes.
	if _, ok := query["events"]; ok {
		switch {
		case res.key == "":
			s3.HandleBucketEventsForBucket(w, r, res.bucket, bucketPath)
		case res.isCollection:
			// A nested collection: never the key-scoped surface (bughunt
			// L2). ?events&versions stays bucket-scoped — the derived
			// version listing is a bucket document.
			if _, versions := query["versions"]; versions {
				s3.HandleBucketEventsForBucket(w, r, res.bucket, bucketPath)
			} else {
				s3.HandleCollectionEventsForBucket(w, r, res.bucket, bucketPath)
			}
		default:
			s3.HandleObjectEventsForBucket(w, r, res.bucket, res.key, bucketPath)
		}
		return true
	}
	// ?versions on a FILE GET: the key-scoped derived listing as JSON.
	// On a collection the param stays ignored (pre-leaf behavior).
	if _, ok := query["versions"]; ok && res.key != "" && !res.isCollection {
		s3.HandleObjectVersionsForBucket(w, r, res.bucket, res.key, bucketPath)
		return true
	}
	return false
}
