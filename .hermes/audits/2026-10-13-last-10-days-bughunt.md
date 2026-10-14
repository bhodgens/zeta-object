# bughunt - last 10 days of commits (2026-10-03 .. 2026-10-13)

Window anchor: `7d82e3f..HEAD` (a0f6b86). 164 commits, 465 files,
+54,734 / -841 lines. Audited 2026-10-13.

## Method

- Baseline gates, both modules, run before any finding was written:
  - root: `make test` PASS; aggregate coverage 82.2% (floor 78,
    `Makefile:25`).
  - `zeta-cache/`: `CGO_ENABLED=0 go test -count=1 ./...` PASS, 8/8 test
    packages ok.
- 7 read-only auditors, one per disjoint scope (sync+index; scheduler+
  transport; fusefs+ipc+config+wiring; Swift FileProvider+macOS;
  s3+fsbackend; webdav/h3/ftp/admin+auth; test/CI infra). Delegation
  `deleg_f6ae39d5`.
- The parent re-read and confirmed every HIGH finding against the source,
  by `file:line`, before it entered this report. Two auditor claims were
  corrected in the process (see "Calibration" below).

## Headline

The window's dominant defect class is the TEST-WIRED-BUT-PRODUCTION-DEAD
SEAM, and it is concentrated in the NEW `zeta-cache` module (~20k lines,
leaves 01-08). Six independent instances:

| # | seam | production behavior | test double that hides it |
|---|------|--------------------|---------------------------|
| H1 | `index.Open` disk probe | nil -> "everything absent" | every test passes a real probe |
| H2 | `Client.Put` create precondition | empty etag -> no header (overwrite) | `memfs` treats empty etag as create-only |
| H4 | scheduler pause channel | closed-and-replaced; loops hold the stale one | tests close the channel once |
| H11 | cursor save | saved inside `Delta`, before apply | stub feed has no apply step |
| M-19 | `fusefs.Options.Uploader` | never set by `main.go` | tests set it |
| H6/H7 | `cacheURL(ofKey:)` | missing `files/` prefix | Swift tests hand-write the wrong path |

ROOT CAUSE OF THE BLIND SPOT: e2e case 40's FUSE mount section is
FUSE-GATED and SKIPS on CI (`scripts/e2e/cases/40-zeta-cache.sh:434-441`;
CI runners have no `/dev/fuse`, dev macOS has no macFUSE by default). So
the entire FUSE write path (H5, H6, H7, M-25, M-26, M-19) has NO CI gate
at all, and the non-FUSE defects (H1, H2, H4, H11) pass because the unit
tests use doubles whose semantics differ from the real wiring.

Also missing: `zeta-cache` has no test job in CI (H12), so its 24 test
files never run in the pipeline either.

---

## HIGH (parent-verified)

### H1. `index.Open` wires a nil disk probe -> every daemon start demotes the entire clean hydrated set
`zeta-cache/internal/index/index.go:104-105` -> `:132` -> `:151` -> `:665-666` -> `:715` -> `:742-743`; only caller `zeta-cache/main.go:54`.
```go
func Open(path string) (*Store, error) { return OpenWithDiskCheck(path, nil) }   // :104
if diskCheck == nil { diskCheck = func(string) bool { return false } }            // :665
case hyd == 1 && dirty == 0 && !diskCheck(path): rep.DemotedDry++                 // :715
SELECT path FROM resources WHERE hydrated = 1 AND dirty = 0 AND deleted = 0       // :743 (unrestricted)
```
`applyDemotions`' query is NOT restricted to journal-touched paths, so with an
always-false probe it demotes every clean hydrated row. Trigger: restart the daemon
on any cache that ever downloaded a file. Effect: `hydrated` silently zeroed with no
journal row; `HydratedRows`/`CleanHydratedRows`/quota `Usage` see an empty cache.
Fix: pass a real probe from `main.go` (stat `CacheDir/files/<key>`), and make the nil
sentinel mean "skip the disk column" (treat as present), not "everything absent".

### H2. `Client.Put` with an empty etag sends NO precondition -> create is an unconditional overwrite
`zeta-cache/internal/transport/client.go:460-462`, `:476-481`; `zeta-cache/internal/sync/diff.go:107`, `:126`, `:245`; `zeta-cache/internal/transport/memfs.go:227-229`; `transport.go:59-63`.
```go
func (c *Client) Put(..) { return c.PutOpts(ctx, key, body, etag, false) }   // :461
switch { case ifMatch != "": .. case ifNoneStar: hdr["If-None-Match"] = "*" } // no default -> no header
```
The engine calls `e.upload(ctx, key, "")` for matrix-8 (new local, server never saw
it) and relies on a 412 create-race (`diff.go:248`). The real client sends an
unconditional PUT; the `memfs` double returns `ErrConflict` for the same call, so
every unit test exercises create-only. The `Transport` doc even contradicts both
("empty etag means create unconditionally"). Trigger: two clients create the same
new key concurrently -> the later PUT silently overwrites. Fix: default `Put` to
`If-None-Match: *` when the etag is empty, or expose create semantics on the seam;
align `memfs` and the interface doc.

### H3. webdav `POST ?batch` is authorized as a READ -> a read-only identity can write and delete
`internal/frontend/webdav/dispatch.go:48`, `:58`, `:193`, `:251-262`; `internal/frontend/webdav/batch.go:66`.
```go
if !f.authorize(w, r.WithContext(ctx), identity, res) { return }   // :48
if f.batchDispatch(w, r.WithContext(ctx), res) { return }          // :58
case "PUT", "DELETE", "MKCOL": return true  ... default: return false  // :251-262 (POST -> false)
```
`authorize` runs first and classifies POST as read; `batchDispatch` then calls
`s3.HandleBatchForBucket(...)`, whose manifest ops are put/copy/move/delete and which
performs no grant check of its own. Trigger: identity with `{Read:true}` on bucket B
sends `POST /B/?batch {"operations":[{"op":"delete","from":"k"}]}` -> allowed, delete
executes. The s3 mount is safe (`opForMethod("POST", bucket, "")` -> `OpCreate` ->
`CanWrite`, `s3/dispatch.go:244-246`, `auth/richgrants.go:379-380`). The webdav tests
wire `auth.WildcardIdentity`, so they cannot see it. Fix: reject a `?batch` request
whose identity lacks write before `batchDispatch`, and add a read-only-identity wire test.

### H4. Scheduler pause channel is close-and-replace, but every loop captured it once -> loops spin on a permanently-closed channel
`zeta-cache/internal/scheduler/scheduler.go:168`, `:173`, `:252`, `:266-275`, `:209-210`.
```go
wake: s.wakeCh(),                                   // :252  captured ONCE at Start
if was != v { close(s.stateCh); s.stateCh = make(chan struct{}) }   // :271-272
case <-st.wake: continue // pause-state flip: re-evaluate        // :209-210
```
After the first `Pause()` or `Resume()`, every loop holds the old (closed) channel.
A closed channel is always ready in a `select`, so the eviction loop spins on
`case <-st.wake: continue` at full CPU and only reaches `nightly`/`check` by random
tie-break; the watcher and sync loops spin the same way (sync only while paused).
Trigger: any pause/resume flip. Effect: unbounded CPU burn; eviction and tombstone
pruning stop being reliable. Fix: pass a `func() <-chan struct{}` and re-fetch after
each wake, or keep ONE channel for the process life and broadcast via `sync.Cond`.

### H5. `fileHandle.Flush`'s `committed` latch is never reset -> writes after a Flush are silently dropped (+ fd leak)
`zeta-cache/internal/fusefs/fs.go:718-740` (reroute), `:751-777` (Flush).
```go
if h.direct || h.committed { return 0 }        // :752  latch never cleared
h.stagingPath = staging; h.local = out; h.direct = false   // :737-739 (no Close, no latch reset)
```
Trigger: `Open(O_RDWR)` -> `Write(A)` -> `Fsync` (commits, `committed=true`, reopen
`direct=true`) -> `Write(B)` (reroutes to a new staging generation, `committed` stays
true) -> `close()` -> the second batch of bytes never leaves `staging/`. Same
silent-write-loss class as the already-fixed `10f3357`. The fd opened in `Open` /
reopened in Flush is dropped at `:738` without `Close()` — one leak per reroute. Fix:
reset `committed=false` in the reroute branch and `Close()` the previous `h.local`.

### H6. Swift Replicated extension copies upload bytes to the wrong path (missing `files/` prefix)
`zeta-cache/gui/fileprovider/Sources/ZetaFileProviderReplicated/FileProviderReplicatedExtension.swift:95`, `:97`, `:127`; `zeta-cache/gui/fileprovider/Sources/ZetaFileProvider/FileProviderExtension.swift:123-125`.
```swift
public func cacheURL(ofKey key: String) -> URL { cacheDir.appendingPathComponent(key) }  // :123-125 — no "files/"
try FileManager.default.copyItem(at: url, to: mount.cacheURL(ofKey: key))               // :95
```
The daemon's data plane is `<cacheDir>/files/<key>` and `mark` stats exactly that path,
so a Finder create/edit fails at `mark` and the bytes sit where the daemon never looks.
The legacy path in the same package does it right (`FileProviderExtension.swift:215`).
Fix: add `"files"` to `cacheURL(ofKey:)`, and assert the create ack's `cachePath` equals
the byte destination in a conformance test.

### H7. Swift `modifyItem` writes/marks the stale OLD key after a rename
`zeta-cache/gui/fileprovider/Sources/ZetaFileProviderReplicated/FileProviderReplicatedExtension.swift:117-129`.
```swift
let key = (item as? FileProviderItem)?.publicKey ?? item.itemIdentifier.rawValue   // :117
if changedFields.contains(.filename) { ... try mount.move(key, to: newKey) }       // :119-125
if let newContents { try FileManager.default.copyItem(.., to: mount.cacheURL(ofKey: key)); try mount.mark(key) }  // :127-128
```
`key` is never reassigned to `newKey`, so after a rename the content write targets the
renamed-away path and `mark(key)` resurrects the tombstoned key. Trigger: rename AND
save a file in one Finder operation. Fix: reassign `key = newKey` before the content
write.

### H8. `CompleteMultipartUpload` replaces the versioning/tag sidecar with a from-scratch object
`internal/frontend/s3/multipart_handlers.go:515-520`, `:612`; `internal/frontend/s3/types.go:51-58`.
```go
meta = ObjectMetadata{ContentType: .., ContentLength: .., ETag: .., CustomMetadata: .., LastModified: ..}  // :515
writeFileAtomicJSON(objectMetadataPath, meta, 0644)   // :612 — overwrites the SAME .meta the version store uses
```
`ObjectMetadata` has no `versioning`/`versions`/`currentVersionId` and no `tags`.
`captureCurrentObjectVersion` has call sites only in `object_handlers.go:101`
(PutObject), `batch_endpoint.go:335`, `versioning_bridge.go:39` — none in the multipart
path. Trigger: enable versioning, `PUT k` (v1), tag it, multipart-upload-and-complete
`k` -> the whole version array and tag map are gone and v1's bytes are unrecoverable.
Reproduces without versioning for tags. Fix: mirror `putObjectHandler`'s
capture -> Put -> record gate and read-modify-write the sidecar via `versionedSidecar`.

### H9. webdav MOVE with `Destination` == the request URI deletes the object and answers 204
`internal/frontend/webdav/copymove.go:30-60`, `:185-207`. No source==destination guard exists anywhere in `handleCopyMove`.
```go
dstObj, err := f.be.Put(r.Context(), dstRes.bucket, dstRes.key, rc, srcObj.Size, putOpts)  // :185
if isMove { if err := f.be.Delete(r.Context(), src.bucket, src.key); err != nil { .. } }   // :199-206
```
A same-URI MOVE resolves dstRes == src, overwrites the object, then deletes it, and
returns 204. RFC 4918 §9.9.4 requires 403. Fix: after `resolveDestination`, return 403
when `dstRes.bucket == src.bucket && dstRes.key == src.key`.

### H10. webdav recursive collection DELETE bypasses the versioning delete-marker path
`internal/frontend/webdav/delete.go:73-77`, `:132-136`.
```go
case kindCollection: f.deleteCollection(..)   // :73
for _, key := range keys { if err := f.be.Delete(ctx, bucket, key); err != nil { .. } }  // :132-135
```
The `kindFile` branch consults `deleteMarkerOrPlain` and suppresses the plain delete on
an Enabled bucket; `deleteCollection` calls `f.be.Delete` directly for every key. Trigger:
`DELETE /bucket/dir/` on a versioning-Enabled bucket -> data files destroyed with no
markers and no captured versions. Fix: route each removed key through the same
marker-aware path (or refuse snapshots mode with 409).

### H11. Cursor advances BEFORE the page is applied, and the first-contact cursor is planted before the initial full scan
`zeta-cache/internal/sync/cursor.go:153-159`, `:167-169`; consumer `zeta-cache/internal/sync/engine.go:148-162`; spec `docs/plans/client-cache-2026-10/05-event-cursor.md:36-38`.
```go
f.saveState(ctx, hist); return nil, true, nil        // :155-158  plant before first scan
paths := eventsToPaths(hist.Events); f.saveState(ctx, hist); return paths, false, nil  // :167-169
```
`saveState` is a durable meta write inside `Delta`; `run` calls `Delta` then scans
(`engine.go:148-162`) with no acknowledgment back to the feed. The spec requires the
cursor advance "ONLY after the diff-apply for that page commits (at-least-once; at-most-once
is not)". Trigger (a): the first `SyncOnce` on a fresh cache fails its scan after the plant
-> the initial reconciliation is skipped until the nightly `FullRescan` (up to 24h).
Trigger (b): a crash after `saveState` but before a path is applied drops that page.
Fix: split fetch from commit (return a page token; `Ack` after apply).

### H12. The `zeta-cache` module has no test gate in CI
`.github/workflows/check.yml:48-53` (build only), `:75-78` (lint only), `:247-248` (build for case 40); test jobs at `:185-189` run root packages only.
The coverage matrix comes from the root `go list ./...`, which cannot include a separate
module, and no workflow contains a `go test` for `zeta-cache`. Its 24 `_test.go` files
never run in the pipeline. Fix: add `cd zeta-cache && go test -count=1 -race ./...` plus a
floor and a floor-guard case for the module.

---

## MEDIUM (auditor-reported; parent spot-checked where noted)

- M13. s3: a rich-grant entry that allows `create` but not `write`/`delete` authorizes a
  `POST /{bucket}?batch` whose items delete (`s3/dispatch.go:244-246`,
  `auth/richgrants.go:365-370`). Verified: `OpCreate` is a separable rich op.
- M14. zeta-cache matrix-6 deletes the remote without checking `serverETag == row.ETag`,
  and `Client.Delete` ignores its etag argument (`sync/diff.go:113-119`, `:294-295`;
  `transport/client.go:519`). Verified by read.
- M15. `/logout` is a state-changing POST outside the session/CSRF gate
  (`internal/adminserver/server.go:150-151`, `:293-301`).
- M16. Admin session/CSRF cookie `Secure` is derived from `r.TLS`, so a TLS-terminating
  front-end mints non-Secure cookies (`internal/adminserver/session.go:135-136`, `:213`).
- M17. Tombstone rows never stamp their own mtime, so the grace window collapses
  (`zeta-cache/internal/index/index.go:300-303`; retention `:473-486`). Verified by read.
- M18. The collection-token skip recurses with a fresh `seen` map, so a full scan re-diffs
  every descendant with one PROPFIND per file (`sync/engine.go:439`, `:466`, `:283-285`).
- M19. `Engine` has no single-run guard; the watcher's fallback calls `SyncOnce`
  concurrently with the sync loop (`sync/engine.go:142-146`, `:212-216`;
  `scheduler/scheduler.go:164-166`).
- M20. The watcher's `pollFallback` can never fire while fsnotify is healthy, so new
  subdirectories created at runtime are never re-watched (`scheduler/watcher.go:180-195`,
  `:90-98`).
- M21. `drain`'s return guard is inverted and the branch is unreachable; a cancel mid-drain
  re-arms the debounce instead of stopping (`scheduler/watcher.go:240-245`, `:136-158`).
- M22. s3 `HEAD` never consults the delete marker while GET and webdav HEAD do
  (`s3/object_handlers.go:801-830`). Verified by read.
- M23. Hot-reload *validation* uninstalls the live zfs dataset provisioner and leaves it
  uninstalled for the duration of any hot patch (`config_store.go:746-757`).
- M24. IPC accepts one connection at a time and `Stop()` can hang on a client that never
  sends (`zeta-cache/internal/ipc/ipc.go:186-199`, `:215-217`).
- M25. `localStat` never checks `IsDir`, so after the entry TTL a directory is re-created as
  a regular file node (`fusefs/fs.go:287-288`, `store.go:56`).
- M26. Client open flags (including `O_TRUNC`) are passed to the cache file, so a shell `>`
  truncates the local generation without marking the row dirty (`fusefs/fs.go:607`).
- M27. IPC wire drift: Go emits `"pins":null`, the macOS Swift decoder cannot represent
  `null` and fails (`ipc/ipc.go:286`, `:297`; `gui/macos/Sources/ZetaCacheMenu/IPC.swift:80`).
- M28. `run-one.sh` still reports PASS for a zero-assert case - the `bbbfe20` fix was applied
  only to `run-e2e.sh` (`scripts/e2e/run-one.sh:126`, `:133`, `:146-152`). Verified by read.
- M29. CI floor guard section 1 passes vacuously when `go list ./...` yields nothing
  (`scripts/ci/test-ci-floor-guard.sh:59-71`). Verified by read.
- M30. `make parity-test` cannot fail if the parity tests are renamed or deleted
  (`Makefile:237-239`) - `go test -run` with no match exits 0.
- M31. Lock file names collide across distinct keys, so a LOCK returns a spurious 423 for an
  unrelated object (`internal/frontend/webdav/lockstore.go:63-65`).
- M32. `.gitleaks.toml` exempts the whole e2e tree and every `.md`, and allowlists the live
  default credential as a bare regex (`:32-53`). Verified by read.
- M33. e2e case 13 leaks a temp dir; the sweeper namespace misses `/tmp/e2e-s3req.*`
  (`cases/13-interop-mc.sh:51`; `e2e-cleanup.sh:94`; `lib.sh:61`).
- M34. `assert_eq ... 0 "$(... grep -c ... || true)"` passes when the listing command itself
  failed (`cases/03-objects-errors.sh:61-66`, `cases/05-multipart.sh:70`).
- M35. The zfs-validate scratch-dataset destroy is inline, not in the EXIT trap, so an early
  abort leaves `testpool/zval` and the deployed server up
  (`scripts/zfs-validate/run-zfs-validation.sh:62`, `:2895`).

## LOW

- L36. `make e2e-report-test` / `ci-floor-test` never run in CI (`Makefile:230`;
  `check.yml:251` is the only `make` call).
- L37. Cursor negative-probe cache is written but never read (`sync/cursor.go:281-284`, `:288`).
- L38. `Status().Conflicts` reports the current pass's count, not the pending total
  (`sync/engine.go:174`).
- L39. `DeleteObjects` reads its body unbounded while the sibling `?batch` caps it
  (`s3/object_handlers.go:1786`).
- L40. The `!data` shadow-dir segment is not rejected, so two legal S3 keys can alias one
  data file (`s3/keyvalidate.go:16-18`; `s3/object_paths.go:33-36`).
- L41. Swift extension hardcodes `/tmp/zeta-cache.ipc`, but the daemon's default is
  `$TMPDIR/zeta-cache.ipc` (`FileProviderExtension.swift:42-43` vs `ZetaCacheMenuApp.swift:45`).
- L42. Every non-"no such item" error maps to `.serverUnreachable`, a transient domain-wide
  signal, so permanent refusals are retried forever (`FileProviderItem.swift:147-156`).
- L43. The enumerator passes `nil` to `finishEnumerating(upTo:)`, so change notifications are
  never delivered (`FileProviderEnumerator.swift:36`, `:44`).
- L44. Two contradictory appex entry points ship in one target (`Appex/main.swift` +
  `AppexSupport.swift`); not settled without an Xcode build.
- L45. The appex is sandboxed but derives `cacheDir` from the user's real home
  (`project.yml:63-68`; `FileProviderExtension.swift:40-44`).
- L46. The h3->TCP fallback re-drives a one-shot request body for non-idempotent verbs
  (`transport/client.go:228-237`, `:309-315`).
- L47. `ErrAuthFailed` is produced but consumed nowhere; a TCP 401/403 falls to a generic
  error (`transport/client.go:180`, `:407-419`).
- L48. Dead/nil seams: `fusefs.Options.Uploader` never set (`main.go:210-216`),
  `ipc.DeletedLister` unused, directories recorded as hydrated cache entries
  (`fusefs/fs.go:498`) so eviction tries `os.Remove` on a dir.
- L49. `ipc` `CopyRemoved` echoes the request's confirm flag (`ipc/ipc.go:355-357`).
- L50. Doc drift: `deleteBatchExecutor`'s comment describes the pre-fix behavior
  (`s3/object_handlers.go:1850-1855`); AGENTS.md item 4 still describes the LISTENING
  sweeper gate the code replaced (`e2e-cleanup.sh:109-112`).

## Tests that cannot fail (from the auditors' notes)

- `scripts/e2e/run-one.sh` - zero-assert cases report PASS (M28).
- `Makefile:237-239` - `parity-test` passes with zero matching tests (M30).
- `scripts/ci/test-ci-floor-guard.sh:59-71` - section 1 passes with zero packages (M29).
- `zeta-cache/internal/sync/cursor_test.go:332` - names the un-cursored guard but returns
  from the first-contact branch.
- `zeta-cache/internal/index/index_test.go:570` - seeds `mtime` by hand, hiding M17.
- `internal/frontend/s3/audit_log_test.go:182-185` - asserts nothing.
- `zeta-cache/gui/fileprovider/Tests/.../ProtocolConformanceTests.swift:91-94` - never
  asserts the payload it names.
- `internal/frontend/ftp/dataconn_deadline_test.go:64-65` - the deadline assertions are
  unreachable unless the server happens to answer 425; otherwise it skips.
- `zeta-cache/gui/fileprovider/Tests/.../ReplicatedExtensionTests.swift:166-168` - asserts
  nothing.

## Calibration (where the parent refuted or softened an auditor claim)

- Task 2's claim that the eviction loop "never reaches `<-nightly` or `<-check`" is too
  strong: `select` picks uniformly among READY cases, so timers still fire by tie-break.
  The verified defect is the permanent busy-spin plus unreliable timer wakeups (H4).
- Task 3's `Appex/main.swift` duplicate-entry-point (L44) and the sandbox container
  (L45) are reasoned from source + project config; no Xcode build was run. Reported as
  inconsistencies, not confirmed link/runtime failures.
- Task 6's `autherr` doc claim that s3 delegates is inaccurate (s3 renders its own
  taxonomy) but is not a defect; the sentinel coverage itself is complete.

## Disclosure ledger - what was NOT verified

- No Swift/Xcode build was run; all Swift findings are source-level.
- `make e2e` was not run (read-only audits); the baseline was `make test` only.
- `make check` (race, shuffle, vuln, secrets) was not run.
- No live ZFS (`zfs-meta`) run; the window's ZFS claims in commit messages are taken as
  reported, not re-derived.
- The reference/cross-cutting seams I read myself were `?events&since-id` (s3 +
  webdav bridge), the zeta-cache index/transport/scheduler/fusefs/cursor hot spots, the
  webdav batch + MOVE + recursive-delete paths, s3 CMPU + HEAD, auth op mapping, and the
  CI/scripts gates. Other MEDIUM/LOW items carry the auditor's evidence only.

## Proposed fix wave (needs the user's go-ahead)

Group A - data loss / data corruption, fix first:
H5, H6, H7, H8, H9, H10, H2, H14 (delete guard), M26, M17.

Group B - authorization and security:
H3, M13, M15, M16, M32, M22.

Group C - cache/daemon correctness:
H1, H4, H11, M18, M19, M20, M21, M25.

Group D - gate integrity (each unblocks the rest):
H12 (add a zeta-cache CI test job), M28, M29, M30, M34, M35, L36, M33.

---

## DISPOSITION (updated 2026-10-13, fix wave complete)

- 44 of 50 findings fixed and committed in 11 commits:
  cbff8d0 (index), d8a04d3 (transport+sync), 814d7c0 (scheduler),
  83a137e (fusefs+ipc+main), f03eef7 (Swift FileProvider),
  4dadf9c (webdav), b7ab57d (s3), 534074e (admin), 99c2179 (CI),
  abf547e (webdav test style), d366078 (e2e honest guards + run-one).
- Gates on the committed tree: `make check` exit 0 (build, vet, fmt, lint,
  test, tidy, race, vuln, secrets + the new zeta-cache test target);
  zeta-cache module 8/8; root 21/21; `make e2e` 968 passed / 0 failed /
  2 skipped (ZFS- and console-precondition cases, as designed).
- Side catches during the wave, fixed: run-one.sh's tally handoff was broken
  for every case (always 'bailed, 0 passed'); staticcheck needed rebuilding
  for go1.27 export data; the OCFileID SA4000 pin was rewritten so both
  linters pass without suppression.
- NOT fixed (6): L44 (needs an Xcode build to settle the duplicate entry
  point), L45 (daemon-side app-group container dependency - TODO in code),
  H8's tag-restore error policy (currently fail-closed 500; owner may prefer
  best-effort), L48's DeletedLister removal note (done), plus the two
  documented-minimum items L43 (anchor diff) and L45 already noted.
- Outstanding validation per the AGENTS.md ZFS rule: a live
  scripts/zfs-validate/run-zfs-validation.sh pass on zfs-meta for the H8
  sidecar-versioning path and the webdav versioning changes.
- Adversarial regression review of the fix commits: see the review wave
  findings appended below when complete.

## REGRESSION-REVIEW WAVE (2026-10-13, complete)

Three adversarial reviewers audited the 11 fix commits. 12 findings; all
fixed and committed in 5 follow-up commits:
- b866aee: R1 edit-upload If-Match (MarkDirty preserves the last uploaded
  ETag; UploadFile overwrites edits with If-Match; 412 stays dirty), R2 ipc
  Stop race (track-before-spawn + stopped flag; <2s pinned), R3 cursor Ack
  no longer replants "0" after loss-invalidation, R4 O_TRUNC close fires the
  prompt-upload hook, R5 pre-upgrade mtime=0 tombstones restamped in init,
  R6 watcher claim set ends per-dir watcher stacking.
- 1d471ca: F1 ?delete&batch -> 400 InvalidArgument (closes the
  write-only-grant DeleteObjects path), F2 CMPU records the captured version
  BEFORE the tags restore, plus a found-while-testing fix: sidecarTagStore
  stripped Versioning/Versions from versioned sidecars on every tag write.
- 1d3b112: Swift F1 nested-key double slash (parentKeyForChild; tests now use
  the real trailing-slash dir-identifier shape), F2 directory create no
  longer marks (dirs come from the remote namespace), F3 README bring-up doc.
- eac477f: gocyclo helper extraction (behavior-neutral).
- 7f4a544: FakeClock timer-settle in the backoff test (race flake).

Reviewer claims refuted by deeper analysis (recorded for calibration):
- R1's "dirty forever" trigger was overstated: the watcher poll converges
  dirty rows; the real regression was upload-412 + unmount refusal, both
  fixed.
- F2's "GET fails mid-restore" case was already safe (tags read before the
  rename, fail-closed); only the post-rename Put direction lost bookkeeping.

Final gates on 7f4a544: make check exit 0; make e2e 968/0/2 (exit 0);
zeta-cache module 8/8; root 21/21. Remaining: live ZFS validation on
zfs-meta (AGENTS.md hard rule) for the H8/webdav-versioning paths.

## LIVE ZFS VALIDATION (2026-10-14, COMPLETE - the AGENTS.md closure step)

Host: zfs-meta, rebooted into 6.8.0-142-generic to load the extended-
metadata ZFS module (2.4.99-1, events/events_size properties). testpool
re-imported; scratch testpool/zval recreated fresh (events=on,
events_size=1M) before each run; destroyed by the harness's EXIT trap
afterward (teardown verified: no zval, no leftover server/zmetad
processes; pre-existing Oct-8 fixtures testpool/ds, ds2 untouched).

Run 1 (pre-reboot): could not run - stock 2.2.2 module has no events
property.

Run 2 (post-reboot): 88/90. Both failures were the harness's OWN stale
floor: 'db_schema_version in supported range (5..8)' pinned the
pre-tagging range; the server (and zfs-metadata#13) accept layout 9 /
wire 3 (zmetadMaxDBSchemaVersion=9). Harness updated to (5..9) with the
lockstep pair 9<->3 - a harness staleness fix, not a product defect.

Run 3: 88/90 - the two nested-key checks (insert-time full_path
resolution) flaked once (wire 200 but empty events on the first
force_collect); the run-2 PASS of the identical checks and run-4's PASS
on the same code path make this a collect-timing flake, not a defect.

Run 4 (final, with both harness fixes): VALIDATION PASSED - exit 0.
  main: 90/90 | zbd: 33/33 | mgmt[plain]: 10/10 | mgmt[ds]: 7/7 |
  h3: 27/27 | fix-campaign: 66/66.
The fix-campaign section covers the wave's ZFS-visible changes: H8's
sidecar versioning (capture/record, tag restore, version list) and the
webdav versioning paths, pinned against `zfs events -j` ground truth.

Harness fix commit: run-zfs-validation.sh db_schema range 5..9 +
lockstep 9<->3 (committed with this update).

CAMPAIGN COMPLETE except the push decision (17+1 commits on local main).
