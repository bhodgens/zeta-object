# bughunt - last week of commits (2026-10-07 .. 2026-10-14)

Window: `c7ffe92^..HEAD` (`27450fd`). 75 commits. Audited 2026-10-14.

The last reviewed point before this wave was `7f4a544` (wave-2
fix-efficacy). Fourteen commits after that point had never been
reviewed. Those commits claim to close the wave-2 findings.

Prior reports, not re-filed unless the Oct 14 fix regressed them:

- `.hermes/audits/2026-10-13-last-10-days-bughunt.md`
- `.hermes/audits/2026-10-13-wave2-fix-efficacy.md`

## Method

- Baseline gates, both modules, green before the report was written:
  - root: `make test` exit 0. Aggregate coverage 82.3% (floor 78).
  - `zeta-cache/`: `CGO_ENABLED=0 go test -count=1 -p 2 ./...` exit 0.
    Eight packages with tests passed.
- 7 read-only auditors, one per disjoint scope. Delegation
  `deleg_068706cd`. No auditor wrote a tracked file. `git status`
  was clean at report time. HEAD stayed `27450fd`.
- The parent re-read every HIGH below at file:line before it entered
  this report. Two auditor severities were lowered (see Calibration).
- Some auditors could not run `go test -overlay` (approval denied).
  Those RED proofs are source-level. Auditor A did run overlays.
  `make e2e` and the live ZFS harness were not run.

## Headline

The Oct 14 fix wave closed the three wave-2 HIGH sites it named
(Put no longer closes the caller's file; CopyObject captures and
records a version; the two Swift parent helpers strip one slash).
The same wave, and the week's tagging feature, left six new HIGH
defects. Two of them are in the Swift path the parent-key fix just
made reachable. One is an authorization hole in the batch grant
fix. Two are in ZFS-native tagging (`7d5ce1c`). One is a watcher
leak the dedupe fix made worse.

---

## HIGH (parent-verified)

### H1. A folder lookup strips the trailing slash, so Finder treats every folder as deleted

`zeta-cache/gui/fileprovider/Sources/ZetaFileProviderReplicated/FileProviderReplicatedExtension.swift:52`
calls `mount.item(identifier.rawValue)`.
`zeta-cache/internal/ipc/fileprovider.go:154` looks the path up with
`normalizePath`, which strips the trailing slash (`:127-129`).
Dir rows are stored with the slash
(`zeta-cache/internal/sync/engine.go:485`, `rowKey := dirKey + "/"`).
`zeta-cache/fileprovider_handler.go:143-145` then returns
`no such item: docs` for a folder whose row is `docs/`.
`FileProviderExtension.swift:82-84` maps that string through
`FileProviderErrorMapper` (`FileProviderItem.swift:188-192`) to
`NSFileProviderError.noSuchItem`.

Trigger: the system refreshes a folder that enumerate just returned.
The item identifier carries the slash. The daemon looks up the
slash-stripped file key.

Effect: the system treats the folder as removed and tries to delete
it. `deleteItem` then refuses a trailing-slash identifier
(`FileProviderReplicatedExtension.swift:198-222`, `.cannotSynchronize`),
so the cleanup cannot finish. `fetchContents` (`:69-70`) has the
same strip. W2-H3 made the folders appear. The next `item(for:)`
deletes them from the system's view.

The suite stays green because `testItemForIdentifierResolvesThroughIPC`
uses a file key, and the stub does not strip.

### H2. A folder drag or rename moves the file-shaped key (regression of W2-M11)

`FileProviderReplicatedExtension.swift:164-166` adds a trailing slash
to the destination, then calls `mount.move`. The daemon strips both
sides (`fileprovider.go:202`, `normalizePath`). `guiHandler.Move`
(`fileprovider_handler.go:257-261`) does `db.Get` on the stripped
path.

Trigger: Finder drag or rename of a folder.

Effect: `Move("docs/sub", "other/sub")`. The dir row `docs/sub/` is
not moved. If a file of that name exists, that file is renamed and
tombstoned, and sync pushes a remote DELETE of the file. Before
`e0d4a6d` a folder drag issued no move. The new tests only move
file keys (`docs/a.txt`).

### H3. A prefix delete grant authorizes a batch delete of any key in the bucket

`internal/frontend/webdav/batch.go:103` and
`internal/frontend/s3/dispatch.go:243-244` call
`AuthorizeOp(OpDelete, bucket, "")`.
`internal/auth/richgrants.go:282-283`:

```go
if key == "" {
    return true // bucket-level (listing) query: bucket match suffices
}
```

That shortcut is for listings. A prefix grant such as
`bucket/public/*` matches it. The batch executor runs no per-item
grant check (`batch.go:81-82`; it calls `HandleBatchForBucket`).

Trigger: a rich grant on `bucket/public/*` with delete, then
`POST ?batch` with `{"op":"delete","from":"secret/victim.txt"}`.

Effect: the object is deleted. A single-op DELETE passes the real
key (`webdav/dispatch.go:206`) and returns 403. Both mounts have
the same empty-key check. The W2-M1 fix closed the write-only hole
and opened this one.

### H4. A tagged PUT resolves the object id from a poll-delayed event log

`internal/metadata/zmetad_objectid.go:42-45` selects the newest
`events` row whose `full_path` or `old_full_path` equals the key.
`internal/frontend/s3/object_handlers.go:168-183` writes the object,
then calls `tagStore.Put` in the same request. The data write is a
temp-file rename. zmetad records that rename on its next poll, not
in this request.

Once the dataset has a `sync_state` row, a missing event row is
permanent `ErrNoSuchKey`
(`internal/frontend/s3/zmetadtagstore.go:150-151`), not the
retryable "not polled yet" error (`:123-126`). `s3ErrorFrom` turns
`ErrNoSuchKey` into 404 after the object is already stored.

Trigger: `zfs_native_tags` on, dataset already tracked, PUT with
`x-amz-tagging` (also CopyObject at `object_handlers.go:1797` and
batch copy at `batch_endpoint.go:371`).

Effect: the client gets 404 for a key that exists. A retry after
the next poll can tag the previous inode. The next CREATE or
RENAME for the new inode then drops those tags. The live harness
retries the PUT up to 10 times with `force_collect` between
failures (`scripts/zfs-validate/ztags-checks.py:231-236`). The
comment there names the 500 "not polled yet" case. The same loop
also retries this 404. A client gets one shot.

Not in the Oct 13 reports. Introduced by `7d5ce1c`.

### H5. After a parent-directory rename, the live key cannot be tagged and the old key still can

Same query (`zmetad_objectid.go:42-45`). It matches one path, not
a rewritten child path. The in-repo contract says directory renames
never rewrite history
(`docs/plans/zmetad-provider-2026-09/master.md:32-36`).
`putObjectTaggingHandler` (`internal/frontend/s3/tagstore.go:286-318`)
does not stat the key before `store.Put`.

Trigger: rename a parent directory (WebDAV MOVE of a collection,
or `mv`). Child event rows keep the old `full_path`.

Effect: `?tagging` on the live key (`archive/a.txt`) is NoSuchKey
until something rewrites the file. `?tagging` on the pre-rename
key still matches. That PUT returns 204 and writes tags onto the
object that now lives at the new path.

This is not the object's own rename. The object's own rename is
pinned (`zmetad_objectid_test.go`) and is the intended
`old_full_path` case.

### H6. Directory delete/recreate leaks a watcher per cycle

`zeta-cache/internal/scheduler/watcher.go:151-155` `release` only
deletes the claim. `addSubtree` (`:283-292`) exits, and runs
`stopSub`, only when its event channel closes. That channel closes
only when the `fsnotify.Watcher` is closed (`watcher.go:105-112`).
Nothing closes the Watcher on Remove. The comment at `:331-333`
says the fan-in sees the watch close. The code does not close it.

Trigger: delete and recreate a watched directory.

Effect: one watcher and two goroutines leaked per cycle for the
life of the daemon. Before `101be5c` a stale claim blocked the new
watcher (one dead watcher per path). The fix re-watches and does
not stop the old one, so churn now stacks. The test asserts a
registration count, not a live-watcher bound.

---

## MEDIUM (parent-verified where marked)

- **M1. Upload fd leak from the W2-H1 fix.** `zeta-cache/internal/sync/diff.go:278`
  and `mustOpen` at `:437-442` return an `*os.File` and never close
  it. `do()` no longer closes `req.Body` (that close was the W2-H1
  bug). `io.NopCloser` does not close the file. The FUSE upload path
  has the same shape (`fusefs/store.go`, auditor-cited `:266`).
  Parent-verified the sync path. Auditor A probe: 40 Puts, fd delta
  42, with GC disabled. Effect: one fd per Put until the finalizer
  runs. A burst can hit EMFILE (this shell's `ulimit -n` is 256).
- **M2. `cacheDiskProbe` treats every stat error as absent.**
  `zeta-cache/main.go:271-272` returns false on any `os.Stat` error,
  not only not-exist. A startup reconcile while `files/` is
  unreadable demotes clean hydrated rows. Auditor A reproduced this
  with mode `000` on `files/`. Parent-verified the predicate.
- **M3. CopyObject writes tags before it records the version.**
  `internal/frontend/s3/object_handlers.go:1796-1811`. A `tagStore.Put`
  failure after the backend Put returns 500 and never calls
  `recordForCopy`. The Put already rewrote the sidecar and wiped
  `Versions`. CMPU records first, then tags
  (`multipart_handlers.go:734` then `:751`). PutObject and batch
  copy still have the CopyObject order. Parent-verified the
  CopyObject order.
- **M4. `trustProxyTLS` no longer forces Secure by itself.**
  `internal/adminserver/config.go:158-168` also requires
  `X-Forwarded-Proto` to equal `https` (whole header, not a list).
  `config.admin-server.example.json:49-54` still says the knob
  alone is what keeps the cookie Secure. A proxy that sets the
  knob and omits the header, or sends `https,http`, ships the
  session cookie without Secure. Parent-verified both sites.
- **M5. A trailing byte skips the batch lock check, and the bridge still runs the delete.**
  `internal/frontend/webdav/batch.go:189` uses `json.Unmarshal`,
  which rejects trailing data, and on failure the gate skips
  (`:174-177`). `internal/batchops/batchops.go:173-177` uses
  `json.Decoder.Decode`, which accepts one value and ignores the
  rest. Trigger: a delete manifest of a locked key plus one
  trailing non-space byte. Effect: 200 and the locked object is
  deleted. Clean JSON is 423. Parent-verified both decoders.
  Move and copy items are never lock-checked (`batch.go:128`).
- **M6. read+write without delete destroys the source via MOVE.**
  `internal/frontend/webdav/dispatch.go:293-294` treats only DELETE
  as a delete op. `versioning.go:161` `moveSourceDelete` calls
  `f.be.Delete`. Trigger: ops `read` and `write`, no `delete`,
  MOVE `/a.txt` to `/b.txt`. Effect: the source is deleted or gets
  a delete marker. DELETE of the same key is 403. Parent-verified.
- **M7. Case 40 lastSync poll never waits.**
  `scripts/e2e/cases/40-zeta-cache.sh:261`.
  `grep -o '"lastSync":[0-9]*'` matches zero digits, so
  `"lastSync":null` yields `"lastSync":` and the loop breaks.
  `assert_contains` of `"lastSync":1` (`:265`) also matches any
  unix timestamp that starts with 1. Parent-verified the pattern.
  This section is outside the FUSE skip, so it runs on CI.
- **M8. `SweepStaging` drops the error it records.**
  Auditor B, `zeta-cache/internal/fusefs/store.go:359-368`: a
  `Remove` failure is stored in `err`, then the function returns
  `nil`. `main.go:58` never takes the warning branch. Parent did
  not re-read that function this turn. Treat as auditor-cited
  until a fixer opens the file.
- **M9. Swift IPC still discards a response whose `tombstones`, `conflicts`, or `entries` is JSON null.**
  Pins were normalized. Those three fields were not.
  `zeta-cache/gui/fileprovider/Sources/ZetaFileProvider/IPC.swift`
  (auditor: `:109-119`). HEAD daemon emits `[]`, so a paired
  deploy does not hit it. An old daemon does.
- **M10. A socket-down error on replicated `item(for:)` skips the error mapper.**
  `FileProviderReplicatedExtension.swift:57` passes the error
  through. `IPCClient.ask` throws `IPCClient.Failure` before the
  mapper (`FileProviderExtension.swift:82-84` only maps a returned
  response). A domain other than the FileProvider domain is a
  transient retry. Trigger: daemon socket down.

## LOW / residual (not re-verified in full)

- Daemon wiring tests call `cacheDiskProbe` and `sweepStagingAtStart`
  directly. Deleting the calls in `main()` leaves those tests green.
- `TestR1EditUploadUsesPreEditETag` still asserts the memIndex
  double. The real pin is `TestAdapterMarkDirtyPreservesETag`.
- `TestR2StopWithPostAcceptPause` still does not hold the
  Accept-to-trackConn window during Stop.
- Case 13's trap still omits four temp files (auditor F). Case 19's
  named leak is closed.
- Case 40's FUSE upload polls still sit inside the CI skip. A skip
  is not a gate. Not a new class.
- W2-M12 is PARTIAL by design: folder delete is refused, not
  implemented. The refusal is complete for the trailing-slash
  identifier production emits. A dir id without the slash still
  takes the file delete. Enumerate emits the slash.
- CMPU's object lock still does not cover the rename
  (`multipart_handlers.go:482` unlocks before `finalizeComplete`).
  The lying-sidecar-on-rename-failure case is closed. The race is
  the lock fact W2-M7 already cited.
- `zmetad_db.go:156-161` doc comment still says schema version == 5.
  The executable gate is a range through 9. Not a runtime bug.
- Auditor G's REMOVE-row claim is not confirmed. The query has no
  `event_type` filter. The comment at `zmetad_objectid.go:16-19`
  says a REMOVE deletes forward identity so the query cannot match
  a deleted object. Those two statements disagree. No test inserts
  a REMOVE row. Not filed as a defect.

## Closed at the named sites (wave-2)

Parent-read, not a full pre-fix replay except where an auditor ran
an overlay:

| id | verdict |
|---|---|
| W2-H1 | CLOSED. `client.go:395-398` wraps the body in `io.NopCloser`. `do()` sets `GetBody` and does not Close. Auditor A overlay of `650d455^` failed with `file already closed`. HEAD pin also reads the caller's file after Put. The fd leak is M1, not a reopen of H1. |
| W2-H2 success path | CLOSED. Capture before Put (`object_handlers.go:1780`), record after (`:1810`), record failure returns before the 200. Tag-store failure before record is M3. |
| W2-H3 helpers | CLOSED. `parentDirKey` and `parentKeyOf` strip one slash. The enumerator calls them. H1 and H2 are the next layer, not a reopen of the helper. |
| W2-M1 write-only | CLOSED for the stated criterion. A write grant without delete is 403. H3 is the prefix-grant hole. |
| W2-M2 nil probe | CLOSED. `main.go:71` passes `cacheDiskProbe`. Nil still means present. M2 is the stat-error half. |
| W2-M4, M5, M8, M9, M10 (IPC null pins), M13, M14 | CLOSED at the sites the auditors quoted. See their notes for line numbers. |
| W2-M3 DELETE and LOCK/UNLOCK | CLOSED. MOVE-as-delete is M6. MKCOL stays write-only, by the leaf-04 doc. |
| W2-M12 | PARTIAL. Refused, not implemented. See LOW. |

## Calibration

- Task B called the watcher leak MAJOR and did not run a live fd
  count (overlay denied). The parent rated it HIGH from the code
  structure: `release` does not `Close` the Watcher, and the
  channel closes only on `Close`.
- Task E's cookie change is a contract change, not a missing
  check. The example was not updated. Filed as M4, not HIGH.
- Task G's harness-retry sentence over-attributes the comment in
  `ztags-checks.py`. The loop does retry non-200, including the
  404. The comment names the 500 case. H4 stands on the resolver
  and the Put-then-tag order, not on that comment.
- Task D could not run the pre-fix overlay. W2-H2 closure is from
  the parent read of the call order, plus HEAD tests that passed
  in that auditor's package run. The pins are not empirically
  proven RED on `24cda6c^`.

## Disposition (2026-10-14 fix wave)

Parent re-read the HIGH sites after the fixers returned. Package tests
passed. No commit of a sibling file. Live ZFS was not run.

| id | status |
|---|---|
| H1, H2 | FIXED. `preserveDirSlash` keeps one trailing slash for item, move, and download. A slash delete returns `folder delete is not supported` and does not call Delete. |
| H3 | FIXED. `BatchMutatingSourceDenied` checks each delete and move source key. The empty-key check remains only as a coarse write-only gate. |
| H4, H5 | FIXED on Linux. A live file's `st_ino` is the object id. A missing file is NoSuchKey and does not read the event log. Non-Linux still uses the event log when the file exists, because `st_ino` is not a ZFS id there. |
| H6 | FIXED. `release` calls the watcher's stop func. `w.Close` is `sync.Once`. |
| M1 | FIXED. Callers `defer f.Close()` after Put. `do()` still does not close `req.Body`. |
| M2 | FIXED. Only `os.IsNotExist` reports absent. |
| M3 | FIXED. Put, Copy, and batch copy record the version before tags. |
| M4 | FIXED. Knob alone is Secure. A present header uses the first value only. |
| M5 | FIXED. `dec.More()` rejects trailing data. A batch move of a locked key is 423. |
| M6 | FIXED. MOVE requires OpDelete on the source. COPY and MKCOL unchanged. |
| M7 | FIXED. The poll requires at least one digit. |
| M8 | FIXED. `SweepStaging` returns the Remove error. |
| M9 | FIXED. Present JSON null on tombstones, conflicts, and entries decodes as `[]`. |
| M10 | FIXED. `item(for:)` maps `IPCClient.Failure` into the FileProvider error domain. |
| Case 13 trap | FIXED. The EXIT trap names the temp files. |
| CMPU lock vs Put | NOT FIXED. Put and CMPU use different lock tables on purpose. Merging them can deadlock. Left as the residual W2-M7 race. |
| Case 40 FUSE skip | NOT FIXED. CI has no `/dev/fuse`. A skip is not a gate. No alternate runner was added. |
| REMOVE-row tag claim | NOT FILED. The query and the comment still disagree. No test pins a REMOVE row. |

## Disclosure

- `make check` (race, shuffle, vuln, secrets) was not run.
- `make e2e` was not run. No live ZFS run. H4 on a real dataset still
  needs `scripts/zfs-validate/run-zfs-validation.sh` on `zfs-meta`.
- Swift: the fixer reported fileprovider 53 tests and macos menu 13
  tests, 0 failures. The parent did not re-run Swift.
- Root package tests for s3, webdav, batchops, adminserver, and
  metadata passed. zeta-cache scheduler, sync, fusefs, ipc, and the
  module root passed.

