# bughunt wave 2 - fix-efficacy + defect audit (2026-10-13, ~23:36-23:50)

Anchor: HEAD `7f4a544` (16 commits ahead of origin/main). Window: the 16
post-audit commits, i.e. the previous wave's fix commits plus its
regression-review follow-ups.

## Method

- Baseline gates run before any finding was written (all green):
  - root `make test` PASS, aggregate coverage 82.3% (floor 78, `Makefile:25`).
  - `zeta-cache/`: `CGO_ENABLED=0 go test -count=1 ./...` PASS, 8/8 packages.
- 6 read-only auditors, one per disjoint scope (index+transport+sync;
  scheduler+watcher+fusefs+ipc; s3; webdav+admin+auth; Swift FileProvider
  +macOS gui; CI/e2e/script gates). Delegation `deleg_5a15ae75`.
- Each auditor did three things per assigned finding: (1) verdict
  CLOSED/PARTIAL/OPEN against the CODE AT HEAD, (2) checked whether the
  added test fails on the pre-fix code, (3) hunted new defects in the same
  code. Every "RED pre-fix" claim below was produced by replaying pre-fix
  sources from git into /tmp trees or `-overlay` (git stash was forbidden).
- The parent re-verified all 3 HIGH findings itself, at file:line, and
  reproduced HIGH-1 independently (see proof).
- No tracked file was written by any auditor. Notes:
  `.hermes/audit-notes/wave2-{A..F}-*.md`.

## Headline

**The previous wave's own H2 fix broke the client write path completely.**
Commit `d8a04d3` added a `req.Body.Close()`/re-seek dance to
`transport.Client.do`. For the two REAL production `Put` call sites the
request body IS the caller's `*os.File`, so the dance closes it and every
upload fails. All 3 regression reviewers missed it, every unit test passes
(the doubles pass `strings.Reader`/`bytes.Reader`, for which `net/http`
wraps the body in a separate `NopCloser`), and the only e2e that exercises
the cache write path is FUSE-gated and skips on CI. Transport package
coverage is 43.2%, the lowest in the module.

Fix-efficacy is otherwise good: 43 of the prior wave's findings are
independently confirmed CLOSED with pre-fix RED proofs. 5 are PARTIAL and
2 are OPEN (below). Two NEW defects in the same class the wave fixed
(CopyObject versioning, Swift dir parenting) are also HIGH.

---

## HIGH (parent-verified)

### W2-H1. Regression from `d8a04d3`: `do()` closes the caller's `*os.File` body, so every real PUT fails
`zeta-cache/internal/transport/client.go:410-423` (added by d8a04d3);
call sites `zeta-cache/internal/sync/diff.go:268` (`mustOpen`) and
`zeta-cache/internal/fusefs/store.go:266` (`mustSeek`).
```go
if rs, ok := body.(io.ReadSeeker); ok && body != nil {   // :410
    getBody := func() (io.ReadCloser, error) { if _, err := rs.Seek(0, io.SeekStart); err != nil { return nil, err }; return io.NopCloser(rs), nil }
    req.GetBody = getBody
    if err := req.Body.Close(); err == nil {              // :418  closes the *os.File
        if b, berr := getBody(); berr == nil { req.Body = b }   // Seek on a closed file
    }
}
```
`http.NewRequestWithContext` assigns `req.Body = body` when the body
already implements `io.ReadCloser` - and `*os.File` does. So `:418` closes
the caller's file, `getBody()` then fails (`file already closed`), and
`req.Body` is left as the closed handle. Effect: EVERY upload from the
real client fails - sync matrix-2/8 and the FUSE prompt upload. It is
invisible to the suite: the unit doubles pass byte/string readers, which
`net/http` wraps in a separate `NopCloser`.
Parent repro (independent, /tmp/w2v, faithful copy of `:401-423`):
```
req.Body concrete type          : *os.File
req.Body IS the caller *os.File : true
getBody() after req.Body.Close() FAILED: seek .../body.bin: file already closed
PUT RESULT: FAILED: Put "http://127.0.0.1:51614/bkt/f.bin":
  readfrom tcp ...: read .../body.bin: file already closed
```
Fix: delete the `req.Body.Close()` + re-seek block; keep `req.GetBody`
only (net/http replays `GetBody` itself on a retry, and it never closes
the caller's handle). Add a test that calls `Put` with a real `*os.File`.

### W2-H2. Single-op `CopyObject` never captures a version -> the destination's history and tags are wiped
`internal/frontend/s3/object_handlers.go:1691-1694`; contrast
`putObjectHandler` `:92-108` (state read + `captureCurrentObjectVersion`)
and `:190` (`recordCapturedObjectVersion`).
```go
_, putErr := backendCall(bucketName, func(b backend.Backend) (objectmodel.Object, error) {
    return b.Put(r.Context(), bucketName, objectName, bytes.NewReader(data), int64(len(data)), ...)  // :1691
})
```
`git grep` confirms capture/record call sites exist for PutObject, CMPU
(`multipart_handlers.go:648,722`), batch copy (`batch_endpoint.go:335,376`)
and the versioning bridge - and NOT for the copy handler. The dest `Put`
rewrites the `.meta` sidecar wholesale with a legacy `ObjectMetadata` (no
Versioning/Versions/tags). Trigger: versioning Enabled, `PUT dst` with
`x-amz-copy-source`. Effect: dst's version array and tags are gone and the
prior bytes are unrecoverable. This is the same class as H8, on the one
write path H8 missed. Fix: run the same capture -> Put -> record gate as
`putObjectHandler`.

### W2-H3. Swift: every directory item reports ITSELF as its parent
`zeta-cache/gui/fileprovider/Sources/ZetaFileProvider/FileProviderItem.swift:57-60`
and `.../ZetaFileProviderReplicated/FileProviderReplicatedExtension.swift:188-192`;
production use `.../ZetaFileProvider/FileProviderEnumerator.swift:63,:92`
and `FileProviderReplicatedExtension.swift:54,:74`.
```swift
static func parentDirKey(of key: String) -> String {   // :57
    guard let idx = key.lastIndex(of: "/") else { return "" }
    return String(key[...idx])                          // dir key "docs/sub/" -> "docs/sub/" (itself)
}
```
Dir keys carry a trailing slash (`identifier(forDirKey:)` appends one;
`FileProviderEnumerator.swift:48` uses `dirKey(of:)`). The enumerator
derives each item's parent with `parentDirKey(of: entry.key)`, so for a
directory the "parent" equals its own identifier and nested folders never
surface in Finder. `parentKeyOf` (`:188-192`) has the same flaw (it slices
off the trailing slash, which `identifier(forDirKey:)` then re-adds). The
F1 fix corrected `parentKeyForChild` but not these two helpers. Fix: strip
ONE trailing slash before slicing in both helpers, and add a test that
enumerates a container holding a subdirectory.

---

## MEDIUM (verified by the parent where marked)

- **W2-M1 `?batch` needs only `write`, but manifests delete.**
  `internal/frontend/s3/dispatch.go:255-257` (POST ?batch -> `OpWrite`) and
  `internal/frontend/webdav/batch.go:79`; no per-item grant check exists
  (the comment at `webdav/batch.go:72-74` says the executor runs none).
  Parent-verified by reading. A grant with `write` but no `delete` can
  delete objects through `?batch` on both mounts, while the single-op
  DELETE path correctly requires `OpDelete`. Fix: require `OpWrite`+`OpDelete`
  or check each manifest item's op.
- **W2-M2 nil disk probe makes reconcile R2 production-dead.**
  `zeta-cache/internal/index/index.go:703-705` (nil now means "present")
  with the only caller `zeta-cache/main.go:54` still passing nil
  (`index.Open(cfg.IndexDB)`). Parent-verified by reading. The H1
  mass-demotion is gone, but so is the rule: delete a hydrated file behind
  the daemon and the row stays `hydrated=1` forever, so quota `Usage` sums
  phantom bytes and evicts the wrong files. Fix: wire a real probe in
  `main.go` (stat `<cacheDir>/files/<key>`).
- **W2-M3 webdav grant-vocabulary drift.** `internal/frontend/webdav/dispatch.go:251-262`:
  `requestGrantsWrite` yields only read/write, so `{"ops":["delete"]}`
  works on s3 but 403s on webdav, a write-only grant can DELETE/MKCOL
  there, and LOCK/UNLOCK fall to `default: false` (read) so a read-only
  principal can lock every key it can read and answer 423 to all writers.
- **W2-M4 `ErrSyncInProgress` is not classified benign.** `zeta-cache/internal/scheduler/syncloop.go:67-68`
  counts a benign skip as a failure (30s x2 backoff up to 1h) and
  `zeta-cache/fileprovider_handler.go:179` returns it, which Swift maps to
  `.serverUnreachable` and retries forever.
- **W2-M5 FUSE read-after-write on one handle returns EIO.**
  `zeta-cache/internal/fusefs/fs.go:745-759`: the direct->staging reroute
  opens the staging generation `O_WRONLY`, so `Read` through the same
  handle fails (auditor reproduced EIO). Fix: `O_RDWR`, or keep a second
  read fd.
- **W2-M6 `staging/` is never swept.** `zeta-cache/internal/fusefs/fs.go:857-859`
  and `scheduler/doc.go:48` promise a daemon-start reconcile that does not
  exist. A crash mid-hydrate leaves unbounded staging bytes that the quota
  cannot see.
- **W2-M7 CMPU sidecar/rename ordering is unlocked.** `internal/frontend/s3/multipart_handlers.go:694`
  (bare write) vs `:704` (rename) vs `:721-746` (record + tags) with the
  object lock released at `:482-483`. A rename failure leaves a sidecar
  claiming the new ETag over the old bytes, with no rollback.
- **W2-M8 CopyObject reads the source unbounded.** `internal/frontend/s3/object_handlers.go:1639`
  `io.ReadAll(srcRC)` while its batch twin caps at `batchCopyMaxBytes`.
- **W2-M9 `validateObjectKey` still lacks canonical-form and "." rules.**
  `internal/frontend/s3/object_handlers.go:1432-1477` vs
  `internal/backend/fsbackend/paths.go:164-166` and
  `internal/backend/batchops/batchops.go:130,135`, so `a//b` and `a/b`
  alias one data file on the multipart path (L40 is PARTIAL, not closed).
- **W2-M10 IPC still emits `null` arrays for tombstones/conflicts.**
  `zeta-cache/internal/ipc/ipc.go:394,433,452`: the M27 fix normalised
  `pins` only. The Swift decoders cannot represent `null`
  (`gui/macos/Sources/ZetaCacheMenu/IPC.swift:80`,
  `gui/fileprovider/Sources/ZetaFileProvider/IPC.swift:97`), so a nil
  slice discards the whole response.
- **W2-M11 Swift `modifyItem` ignores `.parentItemIdentifier`.** `FileProviderReplicatedExtension.swift:141-150`:
  drag a file to another folder and no move is issued.
- **W2-M12 Swift folder delete uses the file-shaped IPC delete.** `FileProviderReplicatedExtension.swift:170-183`
  with `internal/ipc/fileprovider.go:127-130,193` and
  `fileprovider_handler.go:229-238`: the slash is dropped and
  `os.Remove` on a non-empty mirror dir fails ENOTEMPTY, else the file
  form is tombstoned instead of the dir key.
- **W2-M13 H12 is only half closed.** `.github/workflows/check.yml:61-70`
  adds a real zeta-cache test job (verified green, 8/8) but with NO
  coverage floor and no floor-guard case; `scripts/ci/test-ci-floor-guard.sh:46,61`
  is hard-wired to the root module. Measured module coverage: transport
  43.2%, fusefs 62.4%, index 67.4% - exactly where the new regression sits.
- **W2-M14 M34 residual.** `scripts/e2e/cases/03-objects-errors.sh:59-74`
  and `cases/05-multipart.sh:69-81` test the listing that fills the
  sentinel with a SECOND invocation; if the captured listing fails and the
  re-run succeeds, the absence assert passes on the failure it exists to
  catch.

## LOW

- Four regression tests in the fix commits cannot fail: `pause_test.go:170-236`
  (H4 - passes 10/10 on `814d7c0^`), `ipc/r2_stop_race_test.go` (passes
  30/30 on `b866aee^`), `scheduler/r6_watcher_dedupe_test.go:28,52-73`
  (re-implements the helper it asserts; does not even compile pre-fix),
  `fusefs/r1_edit_upload_test.go:51` (asserts the `memIndex` double, whose
  `MarkDirty` preserves the etag by construction, not the production
  `indexAdapter.MarkDirty` the R1 fix changed - proved RED at the adapter
  level only).
- `zeta-cache/internal/sync/cursor_test.go:499-514`: the uncursored-guard
  test never reaches the guard (fresh store returns from the first-contact
  branch), so deleting the guard keeps the suite green.
- `zeta-cache/internal/transport/client.go:525-531`: the `ifNoneStar &&
  ifMatch != ""` branch is unreachable and contradicts the doc at
  `:510-512`.
- `zeta-cache/internal/sync/diff.go:113-125`: the M14 comment promises a
  skip that the code does not implement (the guard lives elsewhere);
  `deleteRemote` ignores its etag argument.
- `zeta-cache/internal/fusefs/fs.go:948`: `renameKey`'s destination
  precondition uses `CleanETag` (blank for dirty rows) instead of
  `KnownETag`, so a rename can clobber a dirty row's server generation.
- `zeta-cache/internal/ipc/ipc.go:394`: `tombstones`, `conflicts` nil-array
  (see W2-M10).
- Swift: `createItem` overwrites an existing key with no
  `filenameCollision` (`FileProviderReplicatedExtension.swift:92-100`);
  `ProtocolConformanceTests.swift:91-94` asserts only `ok`;
  `IPC.swift:117-123` decodes-then-discards a key probe (the one build
  warning) and throws on a partial download payload;
  `zeta-cache/README.md:248` still says "31 tests" (39 now).
- `internal/frontend/webdav/dispatch.go:58` vs `:84-98`: `?batch` is
  dispatched before the lock check, so a live WebDAV lock is bypassed by a
  manifest delete.
- `internal/frontend/s3/tagstore.go:290`: `?tagging` body read unbounded,
  unlike every sibling sub-resource.
- e2e/scripts: `cases/13-interop-mc.sh:52-57` names only `$TMPD` in its
  trap (5 other mktemp files leak); `cases/19-webdav.sh:490,507` leaks
  `$W19_RO_BODY` (not in the trap); `run-one.sh:156-163` verdict+handoff
  un-pinned (`test-skip-reporting.sh` pins only run-e2e's copy);
  `check.yml:69-70` inlines the zeta-cache test command instead of calling
  `make zeta-cache-test-race`, so CI and local gates can drift.

## Fix-efficacy table (prior wave)

| verdict | findings |
|---|---|
| CLOSED (pre-fix RED proof at HEAD) | H1 H2 H3 H4 H5 H6 H7 H8 H9 H10 H11 M13 M14 M15 M16 M17 M18 M19 M20 M21 M22 M24 M25 M26 M27(Go) M28 M29 M30 M31 M33 M35 L36 L37 L38 L39 L41 L42 L43 L48 L49 R1 R2 R3 R4 F1(s3) F2(s3) F1(swift) F2(swift) F3(swift) abf547e 7f4a544 |
| PARTIAL | H12 (job exists, no floor), M34 (separate invocation), L40 (validator rules), R6 (claim set never released), M27 (Swift half), L50 (deleteBatchExecutor comment) |
| OPEN | L45 (appex sandbox - documented TODO), L44 (Xcode link path - source-level only, pgrep path unverified) |

Not-fixed items from the prior report stay not-fixed: L44, L45, H8's
tag-restore error policy (currently fail-closed 500).

## Parent calibration (claims I softened or refuted)

- Task B's "R2 is closed" vs Task A's W2-M2: both are right at different
  layers. The ipc Stop race is closed; the new defect is the nil-probe
  polarity flip making the reconcile rule inert. Reported as W2-M2.
- Task C marks M13 CLOSED and Task D reports the write-only-can-delete
  residual as a new finding: not a contradiction. M13's create-only hole
  is closed; the write-only hole is new/surviving. Reported as W2-M1.
- Task E's L44 "CLOSED at source level": the on-disk
  `ZetaFileProvider.xcodeproj` is gitignored and stale (still references
  the deleted `Appex/main.swift`), so the Xcode link path remains
  UNVERIFIED. Registered, not fixed.

## Disclosure ledger

- `make check` (race, shuffle, vuln, secrets) was NOT run this wave.
- `make e2e` was NOT run by any auditor. The e2e harness was validated by
  syntax + targeted script checks only.
- No live ZFS run. The prior wave's outstanding obligation stands: a
  `scripts/zfs-validate/run-zfs-validation.sh` pass on `zfs-meta` for the
  H8 multipart-versioning path and the webdav versioning changes.
- Swift: `swift build` + `swift test` PASS (fileprovider 39 tests, macos
  11 tests). The Xcode appex scheme fails on a stale, gitignored project
  file, not on tracked code - `xcodebuild` was not settled.
- WORKTREE NOTE (not ours): `scripts/zfs-validate/run-zfs-validation.sh`
  carries an uncommitted 3-line edit widening the accepted
  `db_schema_version` range to include "9". No auditor in this wave made
  it. Assume a concurrent session; do not commit or revert it blindly.

## Proposed fix wave (needs the user's go-ahead)

Group A - outages and data loss, fix first:
W2-H1 (client write path, a regression from this wave), W2-H2 (CopyObject
versioning), W2-H3 (Swift dir parenting), W2-M1 (?batch delete), W2-M2
(nil disk probe), W2-M7 (CMPU ordering), W2-M8 (unbounded copy).

Group B - authorization:
W2-M1, W2-M3 (webdav op vocabulary + LOCK/UNLOCK).

Group C - cache/daemon correctness:
W2-M4, W2-M5, W2-M6, W2-M9, W2-M10.

Group D - gate integrity (each unblocks the rest):
W2-M13 (zeta-cache coverage floor + module-aware floor guard),
W2-M14, the four tests-that-cannot-fail, e2e trap leaks.
