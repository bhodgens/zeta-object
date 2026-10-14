# Wave 2-C — s3 frontend fix-efficacy + defect audit (READ-ONLY)

HEAD = 7f4a544. Repo /Users/caimlas/git/mini-s3 (Go root module github.com/bhodgens/zeta-object).
Assigned findings: H8, M13, M22, L39, L40, L50 + F1/F2 (commit 1d471ca).
Assigned fix commits: b7ab57d, 1d471ca, eac477f.

Method: read code AT HEAD (not diffs alone); read pre-fix source with `git show <hash>^:<path>`;
no tracked file modified.

---

## Assigned-finding verdicts

### H8 — CompleteMultipartUpload replaces the versioning/tag sidecar with a from-scratch object — CLOSED
Report: multipart_handlers.go:515-520, :612; types.go:51-58.
HEAD trace of the CMPU sidecar read-modify-write:
- `completeMultipartUploadHandler` (multipart_handlers.go:336-450): after assembly OK, runs
  `captureForCompletion` (:441 -> def :635-658) and `tagsForCompletion` (:444 -> def :667-678).
- `captureForCompletion` reads versioning state (`versionStoreForBucket(bucketPath).State`,
  :636) and, when Enabled, `captureCurrentObjectVersion(bucketPath, objectName)` (:648).
  State-read error -> 500 fail-closed (:637-644); capture error (non-errNoPriorVersion) -> 500
  (:649-656); errNoPriorVersion -> nil capture (create).
- `assembleCompletedObject` builds a bare `ObjectMetadata` (:537-544) — still no version/tag fields.
- `finalizeComplete` (def :692):
  - :694 `writeFileAtomicJSON(objectMetadataPath, meta, 0644)` — bare legacy write (wipes
    Versioning/Versions/CurrentVersionID + tags on the sidecar).
  - :704 `os.Rename(finalTempPath, finalObjectPath)`.
  - :721-729 `recordCapturedObjectVersion` (versioning_handlers.go:329) re-reads the just-written
    (bare) sidecar and writes `Versions = [capturedOld, ...captured.priorEntries]`,
    `Versioning = Enabled`, `CurrentVersionID = old.id` -> VERSION HISTORY RESTORED.
  - :738-746 tag restore `tagStoreFor(bucketPath).Put(objectName, priorTags)` (only when
    len(priorTags)>0). `sidecarTagStore` round-trips the full `versionedSidecar` (tagstore.go:135-180,
    the F2-adjacent fix), so the tag write does NOT strip Versions -> TAGS SURVIVE.
- Tag restore policy at HEAD is **FAIL-CLOSED (500)**: multipart_handlers.go:736-737 comment +
  :740-744 write the 500 and return. (Matches the disposition note that H8's tag policy is
  fail-closed; owner may still prefer best-effort — unchanged at HEAD.)
- versions+tags survive on Enabled buckets and tags survive on Off buckets (snapshot is
  versioning-state-independent). Pinned by bughunt_20261013_test.go H8x3 + postwave F2 tests; all pass.
=> CLOSED (both halves). Residual defects in the same path are NEW-D1..D3 below.

### M13 — create-only rich grant authorized ?batch/?delete as OpCreate — CLOSED
`opForMethod` (dispatch.go:248-274): `?batch` -> `auth.OpWrite` (:255-257), `?delete` ->
`auth.OpDelete` (:258-260); plain bucket-root POST stays OpCreate (:262). Test
bughunt_m13_test.go:36 fails pre-fix (create-only: pre-fix 200, HEAD 403).

### M22 — HEAD never consults the delete marker — CLOSED
`headObjectHandler`: bucketPath :823, `plainObjectDeleteMarker404` gate :824 ->
404 + `x-amz-delete-marker: true` (:825-827). GET parity at object_handlers.go:552-556.
Test bughunt_20261013_test.go:153.

### L39 — DeleteObjects body read unbounded — CLOSED
object_handlers.go:1825 `io.ReadAll(io.LimitReader(r.Body, bucketBatchMaxBytes+1))`, oversize ->
400 InvalidArgument (:1832-1836). Same const as the ?batch surfaces (batch_endpoint.go:45,85;
batch_bridge.go:80). Test bughunt_20261013_test.go:184.

### L40 — !data/.uploads segments not rejected — PARTIAL
Gating landed in all three validators: s3 object_handlers.go:1463-1474; fsbackend
paths.go:139-150; batchops batchops.go:146-155. Tests bughunt_20261013_test.go:213-286 pass.
BUT s3's `validateObjectKey` (object_handlers.go:1432-1477) still lacks the canonical-form rule
(`cleaned != "/"+key`) that fsbackend has (paths.go:164-166) and the "." segment rule batchops has
(batchops.go:130,135-136). So `ValidateObjectKey("a/./b")`/`("a//b")` return nil while
`DefaultKeyValidator("a/./b")` rejects -> the claimed "segment-for-segment" parity (batchops doc
:112-118) does NOT hold for ".". See NEW-D2.

### L50 — doc drift — PARTIAL
(a) AGENTS.md item 4: HEAD text says "ORPHANED (PPID 1 — the shell that launched it is gone)"
    matching the code (added 99c2179) -> CLOSED.
(b) `deleteBatchExecutor` type comment (object_handlers.go:1894-1899) STILL says "the
    versioned-marker dispatch the DELETE handler runs is NOT taken here — DeleteObjects on a
    versioned bucket removes the data outright", but `Delete` (:1929-1938) calls
    `deleteObjectVersionedMarker` FIRST. Stale doc contradicts code -> OPEN. See NEW-D5.

### F1 (1d471ca) — ?delete&batch authz/dispatch mismatch — CLOSED
`conflictingBatchParams` (dispatch.go:280-293) called at :383 in bucketLevelDispatch, before either
routing arm -> 400 InvalidArgument. Extracted behavior-neutrally by eac477f (diff verified). Test
bughunt_m13_test.go:92 fails pre-fix (pre-fix: OpWrite passes, ?delete dispatched, victim deleted 200).

### F2 (1d471ca) — tag restore before version record dropped history — CLOSED
`finalizeComplete` records the captured version at multipart_handlers.go:721-729 BEFORE the tags
restore at :738-746. F2-adjacent: `sidecarTagStore.readSidecar/writeSidecar` round-trip
`versionedSidecar` (tagstore.go:135-180), so a tag Put/Delete no longer strips Versioning/Versions.
Tests bughunt_20261013_postwave_test.go:28 and :94 both fail pre-fix (forced tag-put failure would
leave versions=0; happy path would drop the second entry).

### Test-quality notes
- bughunt_m13_test.go:169,175 use `if w.Code == StatusForbidden { fail }` — pass for ANY non-403
  (400/500 included). Weak "not-forbidden" pin, not "batch executed". Not wrong, but cannot detect a
  handler that regressed to 400/500.
- export_test_surface.go FinalizeComplete now calls `finalizeComplete(..., nil, nil, ...)` -> the
  exported test shim bypasses the H8 capture/restore by construction; no production impact.
- The H8/M13/M22/L39 tests all assert production contracts and would fail on the pre-fix source
  (verified against `git show <hash>^`). No stub-asserting test found among the assigned set.

---

## NEW defects in the touched code

D1 HIGH object_handlers.go:1691-1694 (copyObjectHandler) + fsbackend/object.go:153-161
   Single-op CopyObject is the ONE write path missed by the H8 fix: it never captures/records a
   version, and the destination Put rewrites the sidecar wholesale with a bare `legacyMeta`
   (no Versioning/Versions/tags fields). Trigger: versioning Enabled; `PUT /b/dst` with
   `x-amz-copy-source: /b/src` where dst has history. Effect: dst's whole version array +
   Versioning marker wiped, prior bytes unrecoverable (the exact H8 class). Every sibling captures:
   PutObject (object_handlers.go:101/190), CMPU (multipart_handlers.go:648/722), batch copy
   (batch_endpoint.go:335/376), webdav PUT/COPY/MOVE (webdav/put.go:94, copymove.go:148). No test
   covers s3 CopyObject + versioning. Fix: mirror the PutObject gate in copyObjectHandler.

D2 MEDIUM object_handlers.go:1432-1477 (validateObjectKey)
   Loosest of the three validators: accepts "." segments and non-canonical keys (`a//b`, `a/./b`,
   `.`) that fsbackend (paths.go:164-166) and batchops (batchops.go:130,135) reject. The multipart
   handlers do their OWN path math off validateObjectKey with NO backend re-validation
   (assembleCompletedObject :466, :526), so `filepath.Join` silently folds `a//b` onto `a/b`.
   Trigger: multipart-upload+complete key `a//b` while `a/b` exists. Effect: aliasing/overwrite of
   another key's data + sidecar; batchops<->s3 "segment-for-segment" parity claim is false for ".".
   Fix: add the `cleaned != "/"+key` canonical check + "." segment rejection to validateObjectKey.

D3 MEDIUM multipart_handlers.go:694 vs :704, :482-483
   CMPU sidecar write ordering + locking: (i) the bare `ObjectMetadata` write (:694) precedes the
   rename (:704) while the restore runs only AFTER a successful rename (:721-729, :738-746), so a
   rename failure (ENOSPC/EXDEV) leaves a sidecar describing the never-installed object (new
   ETag/size over the OLD bytes at finalObjectPath) and the captured history permanently dropped;
   the object lock released when assembleCompletedObject returns (:482-483) is NOT held across
   finalizeComplete, and the tags restore takes no lock at all (tagstore.go writeSidecar) -> a
   concurrent reader sees a transiently version/tag-less sidecar. Fix: lock across
   write->rename->record->restore, and restore history on the rename-error path.

D4 MEDIUM object_handlers.go:1639
   Single-op CopyObject reads the source unbounded (`io.ReadAll(srcRC)`) while its batch twin caps
   at batchCopyMaxBytes (batch_endpoint.go:301, explicitly commented as the reason). Trigger:
   `PUT dst` with x-amz-copy-source of a huge object. Effect: whole source buffered in RAM with no
   ceiling -> memory exhaustion; fsbackend's maxPutBytes only rejects AFTER buffering. Fix:
   LimitReader(batchCopyMaxBytes+1) + oversize InvalidArgument.

D5 LOW object_handlers.go:1894-1899 (= L50 stale half)
   deleteBatchExecutor type comment claims the marker dispatch is not taken while Delete takes it.

D6 LOW tagstore.go:290
   putObjectTaggingHandler reads the tag body unbounded while every sibling sub-resource caps its
   manifest (versioning 1<<20 at versioning_handlers.go:89; ?delete/?batch 4<<20). Fix: cap it.

---

## GATES
- `go test -count=1 ./internal/frontend/s3/...` -> ok github.com/.../internal/frontend/s3 5.884s
- `go vet ./...` -> exit 0

## Not verified
- No live ZFS run (agentor.md hard rule) — H8 sidecar-versioning path not validated on zfs-meta.
- D1 not reproduced end-to-end (read-only); verified by exhaustive capture-call-site grep + the
  fsbackend Put sidecar-write read.
