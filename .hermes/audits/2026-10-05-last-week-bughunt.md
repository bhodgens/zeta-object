# Bughunt audit - last week's work (2026-10-05)

Scope: 262 commits since 2026-09-27; 52 non-test code files across six
campaigns (quic-h3-2026-10 tree, management-api-2026-10,
admin-server-2026-10, plus in-window fixes). Baseline verified GREEN in
the parent before dispatch (`go test ./... -count=1`, `make e2e` 842/842,
`make parity-test` PASS, lint 0) - so every finding below is
new-introduced or latent, not pre-existing baseline noise.

Six read-only auditors over disjoint scopes; the parent independently
re-verified every HIGH finding by grep before it entered this report.

## HIGH (5 confirmed, parent-verified)

**H1. webdav/h3/owncloud write-side versioning is dead code in production.**
`internal/frontend/webdav/put.go:80`, `delete.go:46`, `copymove.go:113`,
`versioning.go:133` all gate on `f.bucketPathFn != nil`, but
`WithBucketPathResolver` has ZERO non-test call sites - production wires
only `WithLockStoreRoot(getBucketPath)` (frontends.go:56, :64; h3/frontend.go:95).
Confirmed: `grep -rn WithBucketPathResolver --include='*.go'` returns only the
option definition and its own test files.
Impact: on a `zfs_versioning=Enabled` bucket, a webdav/owncloud/h3 PUT
overwrite records NO version, and DELETE takes the PLAIN path (no marker,
bytes destroyed) instead of the recoverable marker path. The sidecar is
rewritten wholesale, so existing history is destroyed with no error. The
`?events`/`?versions` surfaces still show rich history, so the bucket looks
versioned while nothing is captured. Green tests do not catch it because
every parity test injects `WithBucketPathResolver` itself.
Fix shape: gate on `f.bucketPath(bucket) != ""` (the resolver already falls
back to lockRoot); the read surfaces already call `f.bucketPath()` and work.

**H2. webdav DELETE records a marker but then still serves the bytes.**
`internal/frontend/s3/versioning_handlers.go:408` `plainObjectDeleteMarker404`
is called only from `object_handlers.go:533` (s3). It has no webdav call site;
grep for "marker" in non-test webdav code returns only comments and the write
side. So: PUT `doc.txt` -> DELETE `doc.txt` (webdav, versioned bucket) ->
204, marker recorded, data file survives -> `GET doc.txt` returns 200 with
the old bytes, and PROPFIND still lists it. S3 on the same two requests
gives DELETE 204, GET 404 + `x-amz-delete-marker: true`.
Currently masked by H1 (the marker branch never runs); fixing H1 alone makes
it immediately reachable, so H1 and H2 must land together.

**H3. admin/console bucket create+delete accept an unvalidated name; `../`
escapes the data root.** `admin_wiring.go:282,290` call `bucketmanager.Create`
/ `Delete` with no name validation; `internal/bucketmanager/bucketmanager.go:203`
carries a suppression comment claiming `validateBucketName` was applied (it
exists only in the s3 frontend's `validBucket` gate). `BucketPath` is
`filepath.Join(dataDir, name)`, which cleans `..`. The auditor executed this:
`Delete(ctx, "../srvdata")` removed a directory outside dataDir -> HTTP 200
`{"deleted":true}`. Reachable via the console's `/api/buckets/{name}` route
(`url.PathEscape("..")` is a no-op - `.` is RFC 3986 unreserved). Requires a
valid operator session, which caps severity below unauthenticated traversal.
Fix: validate at the bucketmanager entry (both Create and Delete) so every
caller is covered, and fix the stale `#nosec` comment.

**H4. batch `?batch` copy leaks a file descriptor per `ifMatch` item.**
`internal/frontend/s3/batch_endpoint.go:146-156` opens the source via
`b.Get(...)` and discards the reader into `_` - `fsbackend.Get` returns an
`*os.File` with no finalizer. A 1000-item manifest with `ifMatch` on every
item leaks 1000 fds, persistent until process exit; EMFILE on a low-RLIMIT
host. (Confirmed the discarded reader in the source.)

**H5. every mTLS cert rejection over webdav renders as HTTP 500, not 401.**
`internal/frontend/webdav/dispatch.go:139` lists only the four Basic-auth
sentinels; `CertAuthenticator` returns `ErrCertMissing` / `ErrCertUnknownCN`
(`internal/auth/cert.go:24-25`), which fall through to the 500 branch. A
valid CA-signed cert whose CN is not in the registry is denied (fail-closed
holds) but answered 500 - monitoring and retry paths read an auth rejection
as a server fault. Contradicts `cert.go:17-19`'s own documented contract.

## MEDIUM (8)

- **M1.** `internal/frontend/s3/batch_endpoint.go:182-184` - batch copy drops
  source user metadata and tags that single-op CopyObject preserves
  (no `Metadata`, no `tagStoreFor`); destination sidecar rewritten with an
  empty `customMetadata`. Also passes no `Principal`, so
  `user.zeta.owner`/`user.zeta.writer.*` breadcrumbs are not stamped on
  batch-written objects - the forensic attribution the audit charter exists
  for is silently absent.
- **M2.** `internal/frontend/s3/batch_endpoint.go:100-126` - a move whose
  copy half succeeds and delete half fails reports one `error` while the
  destination holds the bytes and the source still exists; the response
  understates a half-success (no corruption; observability gap).
- **M3.** `internal/frontend/s3/batch_bridge.go:49` vs `batch_endpoint.go:45`
  - the webdav/h3 batch mount applies `validBucket`, the s3 mount does not;
  a traversal-shaped bucket name reaches the exists check on the s3 surface.
  The parity test asserts the surfaces agree - it does not cover this.
- **M4.** `internal/frontend/s3/object_handlers.go:1830-1835` - on a versioned
  bucket, `POST ?delete` hard-deletes where a single DELETE writes a
  recoverable marker. Pre-existing (verified against `5fb1cbb~1`), but two
  delete semantics for the same key is a data-loss footgun; real S3 inserts
  markers here.
- **M5.** `internal/frontend/s3/export_versioning_test_surface.go` has no
  build constraint and is in `.GoFiles` (verified with `go list`) - the
  shipped binary imports `testing` and exports symbols that repoint
  process-global seams (`backendLookup`, `fsRootResolver`, config view) and
  mutate the unguarded `reflinkCloneFn`. Pre-existing pattern (the older
  `export_test_surface.go` is identical), so MEDIUM not HIGH.
- **M6.** `internal/frontend/webdav/versioning.go:70-78` - a state-read
  failure is swallowed as "not versioned", turning a transient EIO or JSON
  parse error into a silent unversioned overwrite. Matches s3 behavior (so
  parity holds), but it is the one fail-open hole in an otherwise
  fail-closed design.
- **M7.** `config_store.go:151-170` - a REJECTED `PUT /config` leaves live
  seams mutated: `applyHotSeams` installs the config view and region BEFORE
  the provisioner step that can fail, with no rollback. Executed by the
  auditor: `Apply({"region":"eu-west-1","zfs_bucket_datasets":true})` returns
  an error (400) while the live SigV4 region is left at `eu-west-1` and
  `GET /config` still reports `us-east-1` - clients signing the old region now
  get `SignatureDoesNotMatch` from a request reported as a no-op. Violates
  "an invalid patch changes NOTHING".
- **M8.** `config_store.go:522` via `buildRegistryFor` - validation writes the
  PROCESS-GLOBAL `richTable` (`internal/auth/richgrants.go:107-116`) before
  the registry swap, so a rejected patch can still flip
  `AuthorizeOp(...)` false -> true. A privilege grant from a request the API
  reported as rejected.

## LOW (selected)

- **L1.** `internal/frontend/webdav/paths.go:82-92` `keySafe` rejects only
  `.`/`..`, not `.metadata`/`.zfs`, so the webdav key validator disagrees
  with s3's and fsbackend's (both reject the reserved segments). The batch
  path sidesteps it by calling s3's validator.
- **L2.** `internal/frontend/webdav/zfssurface.go:61-68` - `?events` on a
  nested collection path routes to the key-scoped surface, whose
  partial-row key match can return another object's events.
- **L3.** `internal/frontend/webdav/copymove.go` - COPY manufactures a
  version record its s3 counterpart never would (s3 CopyObject has no
  capture/record); in reflink mode that extra record can prune prior version
  data under `reflinkRetention: 0`.
- **L4.** `internal/frontend/s3/zfssurface_bridge.go` re-inlines
  `resolveEventsContext`'s validation in three places instead of sharing it -
  drift risk, byte-equivalent today.
- **L5.** `internal/frontend/h3/frontend.go:167` - the h3 client CA pool is
  never reloaded; `POST /auth/reload` refreshes only admin's CA, so a
  revoked device CA keeps authenticating over QUIC for the process lifetime.
- **L6.** `altsvc.go:62-66` - the Alt-Svc wrapper embeds the
  `http.ResponseWriter` interface without an `Unwrap()`, so
  `ResponseController.Flush` is unsupported on every wrapped frontend (the
  s3 multi-range caller's `_ = rc.Flush()` silently degrades).
- **L7.** `internal/frontend/h3/serve.go:82-85` - shutdown closes the UDP
  socket before the graceful drain and passes `context.Background()` rather
  than main's drain context, so a client that keeps sending can stall the
  whole listener-drain fan.
- **L8.** `config.go:267` - `bucketCfg.UnmarshalJSON` drops
  `reflinkRetention`; reachable via `PUT /config`, and a later
  `POST /config/save` persists 0 (keep zero version copies).
- **L9.** `config_store.go:185` - `Persist` writes the config file 0644 while
  it holds real (unmasked) identity secrets.
- **L10.** admin console: `/logout` sits outside the session gate with no CSRF
  check (inert only because both cookies are SameSite=Strict); `localhost`
  is accepted as loopback without resolving; the session map has no
  background sweeper (unbounded growth); `/login` has no throttle.

## Coverage gaps (not findings)

- No e2e/unit case constructs the webdav frontend the way production wires
  it - exactly why H1 survived a green suite.
- No case for an unknown-CN request over h3/webdav (why H5 shipped green);
  none for an expired client cert.
- No HEAD-with-multi-span case (masked in practice - Go discards HEAD bodies).
- `deleteObjectsHandler` reads the XML body with no size cap (the JSON
  surfaces cap at 4 MiB).

## Parent verification log

Every HIGH confirmed by an independent grep in this session (not the
auditor's word): H1 zero non-test `WithBucketPathResolver` call sites; H2
`plainObjectDeleteMarker404` referenced only from s3; H3 no
`validateBucketName`/`validBucket` in the bucketmanager/admin path plus the
stale `#nosec` comment at bucketmanager.go:203; H4 the discarded reader at
batch_endpoint.go:147; H5 the four-sentinel 401 predicate with the two cert
sentinels absent. H3, M7, M8 were additionally executed by their auditor
against real code paths.