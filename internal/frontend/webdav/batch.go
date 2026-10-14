// batch.go — quic-h3-2026-10 leaf 07: the JSON batch surface
// (POST ?batch) on the webdav frontend. A THIN mount: everything below
// the auth/authorize gates (manifest parsing, validation, ordered
// sequential execution, per-item results, the JSON envelope) is the ONE
// implementation in internal/batchops driven through the s3 package's
// bridge (HandleBatchForBucket, batch_bridge.go) — the same pipeline the
// s3 frontend's ?batch endpoint runs, so the wire semantics are never
// duplicated across frontends. The h3 frontend needs NO code: it wraps
// this handler, so ?batch is served over QUIC unchanged.
//
// Dispatch contract (pinned by batch_test.go + batch_parity_test.go):
//   - POST ?batch on any resource with a bucket resolves to the batch
//     surface (mode B root "/" and mode A /<bucket>/ both carry the
//     bucket scope). The combined parity env pins both shapes.
//   - A GET (or any other method) with ?batch is 405 — the sub-resource
//     is POST-only.
//   - A POST ?batch requires WRITE on the bucket (bughunt H3): a manifest
//     executes put/copy/move/delete operations, so the read-classified
//     POST method alone must not be the gate. The grant check runs AFTER
//     the 405s (a GET ?batch stays the mount's 405) and BEFORE
//     HandleBatchForBucket, whose per-item executor performs no grant
//     check of its own — without this gate a read-only identity could
//     execute deletes and writes over the mount.
//   - Auth and grants run BEFORE the batch resolves (the serveHTTP
//     pipeline): an unauthenticated request 401s, an unauthorized one
//     403s — one request = one bucket = one auth decision covering every
//     item. Audit is ONE record per request in the existing 8-key shape
//     (the webdav frontend does not widen it; s3's serveHTTP records the
//     single record for its mount).
package webdav

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/frontend"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// batchDispatch resolves the ?batch POST BEFORE the method switch (the
// same pre-dispatch position leaf 06's zfsSurfaceDispatch takes for GET
// queries). Returns true when it answered the request. res.bucket is
// non-empty on every ?batch path: mode B resolves every path to the
// configured bucket, mode A's synthetic root ("/" with no bucket) is
// skipped — the root has no batch scope.
func (f *Frontend) batchDispatch(w http.ResponseWriter, r *http.Request, res resource) bool {
	if _, ok := r.URL.Query()["batch"]; !ok {
		return false
	}
	if r.Method != "POST" {
		// The sub-resource is POST-only: 405, never a silent
		// fall-through into the plain method switch.
		w.Header().Set("Allow", "POST")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	if res.isRoot && res.bucket == "" {
		// Mode A's root: no bucket scope to batch against — 405 with
		// the method note (the root answers no POST sub-resources).
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return true
	}
	bucketPath := f.bucketPath(res.bucket)
	if bucketPath == "" {
		// Unwired seam (unit-test shape): no batch surface.
		return false
	}
	// H3: a manifest executes put/copy/move/delete, so the batch
	// sub-resource is a WRITE regardless of the POST method (which
	// requestGrantsWrite classifies as read). Enforce Write on the
	// bucket BEFORE HandleBatchForBucket — its per-item executor runs
	// no grant check, so a read-only identity reaching this line would
	// execute deletes and writes over the mount. The serveHTTP
	// authorize pass above checked the METHOD's classification (read
	// for POST); this re-checks the SUB-RESOURCE's classification,
	// the same one-request-two-scope shape as COPY/MOVE's independent
	// destination gate.
	//
	// BUGHUNT W2-M1 (s3-mount parity): a manifest executes put/copy AND
	// delete items, and the executor runs no per-item grant check — so the
	// write check alone still let a WRITE-ONLY grant delete through the
	// mount while the single-op DELETE path required the delete grant.
	// Require OpDelete too (auth.AuthorizeOp directly — AuthorizeRequest's
	// bool vocabulary cannot express a second op). Deletes are the
	// destructive subset: write covers the put/copy items, delete the
	// delete items, and an identity holding both is exactly what the s3
	// mount now requires (dispatch.go's W2-M1 gate), so both mounts make
	// the SAME decision for the SAME identity.
	if err := frontend.AuthorizeRequest(identityOf(r), res.bucket, true); err != nil {
		writeDavError(w, http.StatusForbidden, "")
		return true
	}
	if !auth.AuthorizeOp(identityOf(r), auth.OpDelete, res.bucket, "", time.Now().UTC()) {
		log.Printf("webdav batch: identity %s denied on %s (POST ?batch requires write AND delete grants)",
			strconv.Quote(identityOf(r).AccessKeyID), strconv.Quote(res.bucket))
		writeDavError(w, http.StatusForbidden, "")
		return true
	}
	// BUGHUNT LOW (wave-2, dispatch.go:58 vs :84-98): batchDispatch runs
	// BEFORE the method switch, so DELETE's enforceWriteLock never runs
	// for a manifest delete — a live WebDAV lock was bypassed by ?batch.
	// Refuse the WHOLE manifest when any delete target is locked: the
	// runner's request-error contract (nothing executes on a manifest
	// error, batchops.go:11-14) is honored by pre-checking BEFORE any
	// item runs, so a partial refusal cannot strand earlier items past
	// their lock. A manifest item carries no lock token, so the check is
	// the tokenless variant; the holder sees 423 exactly as a single-op
	// DELETE would answer. Only delete items are gated: the v1 lock
	// contract gates the delete paths (DELETE via enforceWriteLock).
	//
	// The manifest is decoded here independently of the bridge's own
	// decode so the lock gate can name the delete targets. Only a
	// well-formed manifest carries delete targets; a malformed one stays
	// the runner's/bridge's answer (400), which the gate deliberately
	// does not preempt.
	if man, ok := decodeBatchManifest(r); ok {
		for _, item := range man.Operations {
			if item.Op != "delete" || item.From == "" {
				continue
			}
			store := f.lockStoreFor(res.bucket)
			if store == nil {
				// Locking not wired (the nil contract): enforcement is a
				// no-op, the same rule enforceWriteLock applies.
				break
			}
			if err := CheckWriteLockNoToken(store, f.lockKey(resource{bucket: res.bucket, key: item.From})); err != nil {
				if errors.Is(err, ErrLocked) {
					log.Printf("webdav batch: delete of %s is blocked by a live lock (423); nothing executed",
						strconv.Quote(item.From))
					writeDavError(w, http.StatusLocked, "")
					return true
				}
				// A lock-store failure must never silently pass (the
				// same rule enforceWriteLock applies to the single-op
				// paths).
				writeDavError(w, http.StatusInternalServerError, "")
				return true
			}
		}
	}
	// The key validator is the mounting frontend's own rule set. The
	// webdav path parser has already proven the bucket scope; the
	// s3 validateObjectKey (the shared single-op rules, .metadata/.zfs
	// included) governs every manifest key — the SAME validator the s3
	// ?batch mount passes, so both surfaces reject identical keys.
	s3.HandleBatchForBucket(w, r, res.bucket, bucketPath, s3.ValidateObjectKey)
	return true
}

// batchManifestMaxBytes bounds the gate's own body peek. It mirrors the
// bridge's cap (s3 bucketBatchMaxBytes, 4 MiB) with a local copy: the gate
// reads the body BEFORE the bridge does and must reinstall it verbatim, so
// it needs the same ceiling — a body beyond it is rejected 400 by the
// bridge right after the gate, so a pinned local constant cannot widen the
// bridge's limit. (A getBatchMaxBytes re-export from s3 would be the
// alternative; the pin test below keeps the two honest.)
const batchManifestMaxBytes = 4 << 20

// decodeBatchManifest peeks at the ?batch request body WITHOUT consuming
// it: the body is read once (bounded — a body beyond the cap is rejected
// 400 by the bridge right after this gate), the manifest decoded, and the
// body reinstalled verbatim so the bridge's HandleBatchForBucket reads it
// unchanged. Any read or decode failure is NOT a gate error: the manifest
// carries no trustworthy delete targets, so the gate skips and the bridge
// answers exactly as it did pre-fix (its request-error contract: a
// malformed manifest executes NOTHING).
func decodeBatchManifest(r *http.Request) (batchops.Manifest, bool) {
	if r.Body == nil {
		return batchops.Manifest{}, false
	}
	body, rerr := io.ReadAll(io.LimitReader(r.Body, batchManifestMaxBytes+1))
	cerr := r.Body.Close()
	if rerr != nil || cerr != nil {
		return batchops.Manifest{}, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var man batchops.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return batchops.Manifest{}, false
	}
	return man, true
}
