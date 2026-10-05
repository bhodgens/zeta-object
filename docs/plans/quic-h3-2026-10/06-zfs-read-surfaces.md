# Universal ZFS Read Surfaces (events + versions over WebDAV) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the ZFS-enrichment READ surfaces over WebDAV: event
  history (`?events`) and the version listing for a file, served by
  the webdav frontend from the SAME zmetad-backed data the S3
  capability endpoints serve. Completes the universality rule
  (master decision 7) on the read side; leaf 05 covered the write
  side.
- **Dependencies:** leaf 05 (its extraction pattern and committed
  state; coordinate shared-package placement through the
  orchestrator)
- **Estimated Context:** 50K
- **Concurrency Group:** A3 (after 05 commits)

## Goal

A client that speaks only WebDAV (the zeta-cache daemon, Finder, a
browser) can read the same event history and version data an S3
client gets from `?events` and `?events&versions` - identical data,
identical JSON, served from the bucket's own path over the webdav
frontend. The h3 frontend inherits everything for free (it wraps the
webdav handler).

## Context

- The S3 surfaces live in `internal/frontend/s3/capability_endpoints.go`
  (`handleBucketEvents`, the `Events []objectEventJSON` response
  shape at line 139, the `?versions` branch at 244) with dispatch
  hooks in `dispatch.go:316-322` and `:392`. The data source is the
  zmetad provider resolved through main-package seams
  (`zmetadDBFor()`, config view).
- The webdav dispatch is `internal/frontend/webdav/dispatch.go`
  (method switch at line 55; read-method guard at 167). The webdav
  frontend has NO events/versions surface today (verified: zero hits
  for events/versions in the package).
- Placement rule (same decision leaf 01 and 05 faced, and by now the
  tracking table records the outcome): extract the SHARED response
  builders and provider resolution into a leaf package (for example
  `internal/zfssurface/`), re-point s3, and have webdav call it. Do
  NOT duplicate the JSON building or the provider plumbing. If 05
  already created the shared home, extend it.
- Wire shape: JSON (the S3 events shape, byte-identical), NOT XML -
  this is an extension endpoint riding on the webdav path, and the
  universality rule is parity of DATA. Content-Type:
  `application/json`.
- Charter check (answer in the report): these surfaces are ENRICHMENT
  only - they read provider data, never write it; no server-owned
  state is added. The parity gate of the metadata seam
  (internal/metadata/parity_test.go) must stay green: enrichment
  never alters core metadata responses.

## Interface Contracts (From Parent)

### Contract 8: webdav ZFS read surfaces (you produce this)

```go
// internal/frontend/webdav (behavior, not new exported API):
// GET on a COLLECTION path with ?events            -> the same JSON the
//   S3 GET /{bucket}?events returns (identical bytes for identical
//   data), 200 application/json; provider unavailable -> the same
//   honest 503 body the S3 endpoint answers.
// GET on a COLLECTION path with ?events&versions   -> the combined
//   extension, same parity rule.
// GET on a FILE path with ?versions                -> that file's
//   version listing, same data as the S3 ?versions listing filtered
//   to the key (JSON; identical entry shapes).
// A request with NO query params is unchanged byte-for-byte (the
//   existing PROPFIND/GET paths are untouched).
```

- Auth: the webdav frontend's existing authenticator gates these
  GETs (they are ordinary GETs in the dispatch table) - no new auth
  surface.
- h3: no code - the wrapped handler serves the new queries over
  QUIC automatically. One test in the h3 package (or a comment in
  the parity test stating why none is needed) records that.

## Tasks

### Task 1: the parity test first (RED)

**Objective:** pin data parity between protocols before any code.

**Files:**
- Create: `internal/frontend/webdav/zfssurface_test.go`

**Step 1: Write the failing test.** One test bucket, provider
fixture (the same fixture the s3 capability endpoint tests use -
find it with search_files and mirror; if it is main-package, follow
the established seam-var pattern the s3 tests use):
- `GET /<path>/?events` via the webdav handler == the s3 handler's
  response body for the same bucket data (byte compare);
- `GET /<file>?versions` entry set == the s3 listing entries for
  that key;
- provider unavailable -> 503 with the same body the s3 endpoint
  answers;
- `GET /<path>/` with no params -> unchanged (existing golden
  output).
Run -> FAIL (webdav answers these as plain GETs today, or 404s the
query).

**Step 2:** `go test ./internal/frontend/webdav/ -run ZFSSurface
-count=1` -> FAIL.

### Task 2: extract the shared surface package

**Objective:** one implementation, two (three) frontends.

**Files:**
- Create: `internal/zfssurface/` (the response builders + provider
  resolution moved from `internal/frontend/s3/capability_endpoints.go`
  and its helpers; s3 re-pointed, s3 tests UNMODIFIED and passing)
- Modify: `internal/frontend/s3/capability_endpoints.go` (delegate)
- Note: if leaf 05 created the shared home already, extend it and
  skip the new package.
- Floors: code moved packages -> re-measure and update floors in the
  SAME leaf (AGENTS.md).

**Step 1:** Move + re-point; `go test ./internal/frontend/s3/ -count=1`
green UNMODIFIED. **Step 2:** floors re-measured if the move crosses
the floor's package boundaries.

### Task 3: webdav dispatch

**Objective:** the queries resolve on the webdav path.

**Files:**
- Modify: `internal/frontend/webdav/dispatch.go` (GET branch: query
  check BEFORE resource resolution - a query on a collection must
  not fall into the collection-GET path)
- Create: `internal/frontend/webdav/zfssurface.go` (thin handlers
  calling the shared package)

**Step 1: Implement.** The query check must be explicit: `?events`
on any GET (collection or file), `?versions` on a file GET; unknown
query params on GET keep today's behavior exactly (they are ignored
- pin that too, one assert).

**Step 2:** Task 1's parity assertions PASS; full webdav + s3 suites
green.

### Task 4: gates

- `go test ./... -count=1` green;
- `make parity-test` green (enrichment never altered core metadata);
- `gofmt -l` clean on changed files; `make lint NEW_FROM_REV=HEAD`
  -> 0 findings.

## Self-Verification Checklist

- [ ] Parity test RED before implementation, GREEN after
- [ ] Byte-identical JSON between s3 and webdav surfaces for the
      same bucket data
- [ ] Provider-unavailable -> identical honest 503 on both paths
- [ ] No-param GETs byte-unchanged (golden assert)
- [ ] `make parity-test` green
- [ ] No duplication: webdav delegates to the shared package
- [ ] Floors re-measured if code moved packages
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH respected: `internal/metadata/` (the frozen seam),
      `internal/backend/`, `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.mod`/`go.sum`, `internal/frontend/webdav/put.go` and
      `delete.go` and `copymove.go` (leaf 05's files), `get.go`
      (leaf 01's file)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The parity test compares BYTES, not shapes
- [ ] Charter answer present in the report: enrichment-only, no new
      state, parity gate green
- [ ] h3 inheritance stated (no code, or the test that proves it)
- [ ] If extraction happened: s3 tests unmodified from the new
      location; floors updated

Output: APPROVED or specific gaps with file:line.

## Notes

- This leaf is the read half of universality; the client's
  reconnect scan (O(changes) via the event cursor instead of
  O(tree) via full PROPFIND) is the consumer that justifies it.
  The cursor design (zmetad rowid as the sync token) is CLIENT-side
  and belongs to the zeta-cache tree - this leaf ships the data
  access only.
- Do not invent a sync-token mechanism here. RFC 6578
  (sync-collection REPORT) is a future candidate; the ?events JSON
  already carries ordering sufficient for a cursor consumer.
