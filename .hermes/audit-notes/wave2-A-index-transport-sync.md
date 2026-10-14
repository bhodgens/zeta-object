# Wave-2-A audit: zeta-cache index / transport / sync at HEAD 7f4a544 (read-only)

Scope: findings H1, H2, H11, M14, M17, M18, M19, L37, L38 + regression-review R1, R3, R5.
Fix commits: cbff8d0 (index), d8a04d3 (transport+sync), b866aee (regression close-out).
No tracked file was modified. All pre-fix proofs use `go test -overlay` (probe tests and
pre-fix sources live in /tmp/zcw2), so `git status` stays clean and `git stash` was never used.

## Method
1. Read each finding's stated defect in `.hermes/audits/2026-10-13-last-10-days-bughunt.md`,
   then read the CODE AT HEAD and the pre-fix source with `git show <rev>:<path>`.
2. Reproduce the added regression test against the pre-fix source via overlay
   (`{"Replace": {repo/test.go: /tmp/probe_test.go, repo/src.go: /tmp/src_prefix.go}}`) and
   require RED pre-fix / GREEN at HEAD. Green-only-at-HEAD probes were written for findings
   whose own test file could not compile against the pre-fix tree (Ack-less cursor.go, etc.).
3. Hunted new defects in the same files, in the classes the report names.

## CLOSED with proof (RED pre-fix -> GREEN at HEAD)
| finding | proof command (overlay) | RED result |
|---|---|---|
| H1 | ov_index_h1.json (index.go=cbff8d0^) TestNilDiskCheckKeepsHydratedRows | `nil probe demoted "a" ... Hydrated:false` |
| M17 | same overlay, TestDeletePathStampsTombstoneMtime | `tombstone mtime = 1788068416, want within [1791956416,...]` |
| H2 | ov_transport_h2.json (client.go=d8a04d3^) TestPutEmptyETagSendsIfNoneMatchStar | `If-None-Match = "", want *` |
| M14 | ov_sync_m14.json (diff.go=d8a04d3^) TestTombstoneKeptOnRemoteGenerationChange | `the remote file was DELETED despite carrying a foreign generation` |
| H11 | ov_sync_h11.json (cursor.go+changefeed.go+engine.go=d8a04d3^) TestWave2ProbeDeltaDoesNotPersistCursor | `Delta persisted cursor meta (<nil>)` |
| L37 | same overlay, TestWave2ProbeFreshNegativeProbeSkipsFetch | `re-fetched (1 -> 2 calls)` |
| M18 | ov_sync_engine.json (engine.go=d8a04d3^) TestWave2ProbeFullRescanComplementNoExtraPropfinds | `PROPFINDs = 5, want 3` |
| M19 | same overlay, TestWave2ProbeConcurrentSyncRefused | `concurrent SyncOnce ran to completion (no single-run gate)` |
| L38 | same overlay, TestWave2ProbeStatusConflictsPending | `Status().Conflicts = 0 after a clean pass, records = 1` |
| R1 | ov_adapter_r1.json (adapter.go=b866aee^ + KnownETag shim) TestWave2ProbeAdapterMarkDirtyPreservesETag | `MarkDirty lost the last uploaded ETag: ""` |
| R3 | ov_sync_r3.json (cursor.go=b866aee^) TestR3LossAckLeavesCursorAbsentNotZero | `cursor meta after loss Ack = <nil>` |
| R5 | ov_index_r5.json (index.go=b866aee^) TestR5PreUpgradeTombstoneRestampedOnOpen | `pre-upgrade tombstone still has mtime 0` |

HEAD verification (no overlay) of every added test: `CGO_ENABLED=0 go test ./internal/index/...
./internal/transport/... ./internal/sync/...` = ok x3; full module `go test ./...` = 10 pkgs ok.

## Verdicts
- H1 CLOSED. index.go:703-705 `if diskCheck == nil { diskCheck = func(string) bool { return true } }`.
  The stated defect (mass demotion at daemon start) is gone; applyDemotions (index.go:774-806) now
  only fires for rows whose probe says absent. Residual (NEW, finding 2): the daemon still passes nil.
- H2 CLOSED. client.go:506 `Put(...) = PutOpts(..., etag, etag == "")`; PutOpts:525-527 defaults to
  `If-None-Match: *`; the interface doc (transport.go:60-70) and memfs (memfs.go:224-234) now agree.
- H11 CLOSED. cursor.go Delta never writes state (137-225); engine.go:208-210 `acker.Ack(ctx)` only
  after a successful pass; Ack is the sole writer (cursor.go:247-259).
- M14 CLOSED for the reachable path: diff.go:161 `if serverETag != row.ETag { keep + recordConflict }`
  before `deleteRemote`. Delete documents "no wire precondition" (transport.go:74-81) and the wire pin
  TestDeleteSendsNoPrecondition holds. Doc/behavior mismatch at diff.go:113-125 -> NEW finding 6.
- M17 CLOSED. index.go:333-338 stamps `mtime = time.Now().Unix()` on insert and conflict.
- M18 CLOSED. One `seen` map threaded: engine.go:319 -> 425 -> 460 -> 490/520 (no `map[string]bool{}`
  per subtree anywhere).
- M19 CLOSED. engine.go:167-170 `if !e.running.CompareAndSwap(false,true) { return ErrSyncInProgress }`
  held for the whole pass, released by defer. Caller handling is a NEW defect (finding 3).
- L37 CLOSED. cursor.go:149 `if f.ProbeFresh(ctx) { ... return nil, true, nil }`; probeStampedAt (347)
  is read again. (The old test comment "future work may skip the fetch" is stale at cursor.go:367.)
- L38 CLOSED. engine.go:281-286 Status derives from pendingConflictCount (305-311); the retired
  counter/meta row are gone. `e.conflicts` + snapshotConflicts (245-249) are now dead but doc'd.
- R1 CLOSED (production): adapter.go:60-77 MarkDirty preserves prev.ETag; fusefs/store.go:265 uses
  KnownETag for If-Match. Test-efficacy gap -> NEW finding 4.
- R3 CLOSED. cursor.go:247-253 writes the cursor only when `pendingID > 0`.
- R5 CLOSED. index.go:150-161 restamp, called from init() at :172-176 before the reconcile pass.

## NEW defects (detail; numbered as in the returned report)
1. HIGH transport/client.go:411-420 - `do()` closes the caller's body.
   `req.Body` for an *os.File body IS the file (net/http assigns ReadCloser bodies directly), and
   `http.NewRequestWithContext` gives no GetBody for that shape. The block does
   `req.Body.Close()` then `getBody()` -> `rs.Seek(0)` fails on the closed file -> `req.Body` is left
   closed. BOTH production PUT call sites pass an *os.File: sync/diff.go:268 `mustOpen(local)` and
   fusefs/store.go:266 `mustSeek(local)`. Probe (HEAD, no source overlay):
   `go test -overlay=/tmp/zcw2/ov_osfile.json -run TestWave2ProbePutOSFileBodyReaches ./internal/transport/`
   -> `Put(*os.File) = Put "https://127.0.0.1:.../bkt/f.bin": read <tmp>/body.bin: file already closed`.
   Invisible to the suite: unit tests use strings/bytes readers (Close is a no-op) and memfs never
   receives a file body; case 40's daemon upload path has no live gate.
   Fix: drop the Close/replace dance (keep at most `req.GetBody`), and let the h3->TCP fallback
   `Seek(0)` before re-driving.
2. MEDIUM main.go:54 wires `index.Open` (nil probe) -> R2 is dead in production (see H1 residual).
   Effect: quota.go:97-106 Usage sums rows whose file is gone -> premature eviction of other files;
   adapter.HydratedCount over-reports. Fix: `index.OpenWithDiskCheck(cfg.IndexDB, stat(<cacheDir>/files/<key>))`.
3. MEDIUM syncloop.go:67-68 counts ErrSyncInProgress as a failure (30s x2 -> 1h backoff) and
   fileprovider_handler.go:179 returns it to Finder ("download X: sync: a pass is already running").
4. MEDIUM r1_edit_upload_test.go:51 asserts memIndex (memindex_test.go:46-54 never clears the etag),
   not the production indexAdapter.MarkDirty that R1 changed; adapter_test.go has no pin.
5. LOW client.go:525-531 - unreachable `if ifNoneStar && ifMatch != ""` + false doc at :510-512.
6. LOW diff.go:113-125 - comment promises a tombstone/listing etag guard the branch does not implement.
7. LOW fs.go:948 - rename destination precondition still uses CleanETag (blanks dirty rows).
8. LOW cursor_test.go:499-514 - the un-cursored test never reaches cursor.go:207-211.

## Gates
- `cd zeta-cache && CGO_ENABLED=0 go test -count=1 ./internal/index/... ./internal/transport/... ./internal/sync/...` = ok (3/3)
- `cd zeta-cache && CGO_ENABLED=0 go test -count=1 ./...` = ok (10/10, 0 fail)
- `cd zeta-cache && go vet ./internal/index/... ./internal/transport/... ./internal/sync/... .` = exit 0, no output
- 12 overlay RED/GREEN proof runs as tabulated above; 1 HEAD RED probe (finding 1).
- NOT run: make e2e (forbidden), make check, ZFS validation on zfs-meta.
