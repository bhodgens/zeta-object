# Fix-wave regression audit - zeta-object wave b433eb1~1..b150a85 (2026-10-05)

Scope: the 12-commit fix campaign that closed
`.hermes/audits/2026-10-05-last-week-bughunt.md` (H1-H5, M1-M8, L1-L9).
Baseline verified GREEN in the parent BEFORE dispatch: `go test ./... -count=1 -p 4`
= 21 packages ok / 0 FAIL (log `/tmp/bughunt_baseline_unit.log`);
`make e2e` = 842 passed / 0 failed / suite exit 0 (log `/tmp/bughunt_baseline_e2e.log`).
So every finding below is newly introduced by the wave or a latent hole the
green suite does not reach.

Five read-only auditors over disjoint scopes (webdav/h3/owncloud, s3+batchops,
config/admin/auth, bucketmanager+fsbackend, evidence-integrity). The parent
independently EXECUTED a proof for every CRITICAL and HIGH before it entered
this report. Report-only: no fixes applied.

## Verdict

The wave closed its targets. Every fix it claims to have closed is closed, and
each has a test that fails on revert of the production file alone (the
revert-matrix technique: revert production files, KEEP the new tests).

It also introduced or left one CRITICAL data-loss/integrity hole, two HIGH
state-honesty holes, one HIGH data race, and a verification harness that cannot
report a failure it did not count.

| Severity | Count | New this wave | Pre-existing |
|---|---|---|---|
| CRITICAL | 1 | 0 | 1 (H3 class, unfixed sibling) |
| HIGH | 3 | 3 | 0 |
| MEDIUM | 12 | 8 | 4 |
| LOW | 14 | 10 | 4 |

---

## CRITICAL

### C1. A traversal-shaped bucket name escapes the data root on every
### ungated s3 bucket and object surface. `..` deletes and overwrites files
### OUTSIDE `dataDir`.

`internal/frontend/s3/object_handlers.go:1758` (DeleteObjects),
`:883` (listObjectsV2), `internal/frontend/s3/versions_listing.go:59`,
`internal/frontend/s3/multipart_handlers.go:772`, and the whole
`objectLevelDispatch` method switch (`internal/frontend/s3/dispatch.go:434-445`)
gate only on `bucketExists(bucketName)`, never `validBucket`:

```go
func deleteObjectsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !bucketExists(bucketName) {
```

`bucketExists` -> `getBucketPath` -> `filepath.Join(cfg.DataDir, bucketName)`
(`internal/frontend/s3/bucket_handlers.go:33`), and `filepath.Join` CLEANS
`..`, so `getBucketPath("..")` is `dataDir`'s PARENT.

This is exactly the H3 hole the wave closed at the bucketmanager layer in
`50bc4cd`, left open one layer up: `50bc4cd` validated `bucketmanager.Create`
and `.Delete`, but the s3 FRONTEND's own object plane never calls those for
these surfaces - it calls `fsbackend` through the `backendLookup` seam, which
joins the raw name. `7a8ad1b` fixed the sibling `?batch` surface and stopped.

**EXECUTED PROOF (parent, overlay probe, production backend shape - an
undeclared bucket name falls through `buildBackendLookup`'s default branch to
`constructor(fs, dataDir)`, `backend_lookup.go:138`, i.e. an FS backend ROOTED
AT dataDir with no single-bucket opt, which is what production builds):**

```
?delete bucket=.. key=victim.txt              status=200
   >>> VICTIM OUTSIDE dataDir WAS DELETED
PUT     bucket=.. key=planted.txt             status=200
   >>> FILE WRITTEN OUTSIDE dataRoot: /tmp/.../001/planted.txt content="planted by s3"
DELETE  bucket=.. key=secret2.txt             status=204
   >>> FILE OUTSIDE dataDir WAS DELETED
GET     /..?list-type=2                       status=200
GET     /..?versions                          status=200
GET     /..?uploads                           status=200
?batch  bucket=.. (gate added by 7a8ad1b)     status=404
```

`http.ServeMux` does NOT save you: it cleans `/..` to a 307 redirect, but
percent-encoded forms survive to the handler. Parent probe against a real
`http.ServeMux`:

```
target="/%2e%2e?delete"      status=200 path="/.."        raw="/%2e%2e?delete"
target="/%2E%2E?delete"      status=200 path="/.."        raw="/%2E%2E?delete"
target="/..%2f?delete"       status=200 path="/../"       raw="/..%2f?delete"
target="/%2e%2e/victim.txt"  status=200 path="/../victim.txt"
```

So the triggering request is a SigV4-signed `POST /%2e%2e?delete` with body
`<Delete><Object><Key>victim.txt</Key></Object></Delete>`, or a plain
`PUT /%2e%2e/<key>`. Auth still applies (a valid identity with a `*` or
matching grant is required), which caps this below unauthenticated traversal -
but any authenticated client escapes the data root.

Fix shape (parent-verified sites):
1. Gate `objectLevelDispatch` once, at `internal/frontend/s3/dispatch.go:374`,
   next to the identity publish - that closes PUT/GET/DELETE/multipart/tagging
   in one move and no future surface can skip it.
2. Gate `deleteObjectsHandler` (`object_handlers.go:1758`),
   `listObjectsV2Handler` (`:883`), `listObjectVersionsHandler`
   (`versions_listing.go:59`), `listMultipartUploadsHandler`
   (`multipart_handlers.go:772`) with
   `if !validBucket(bucketName) || !bucketExists(bucketName)`, matching the
   `?batch` shape at `batch_endpoint.go:54`.
3. Pin each with an e2e case under `scripts/e2e/cases/` (AGENTS.md rule).

### C2. (folded into C1 - same gate, one mechanism.)

---

## HIGH

### H1. A hot `PUT /config` co-patched with a restart-required key installs
### that key into the live data plane while `GET /config` reports the old value.

`config_store.go:213` -> `s3_wiring.go:196-209`: `applyHotSeams(&candidate)`
installs the WHOLE s3 config view (`Buckets`, `DataDir`, `AuditReads`,
`ReflinkRetention`) from `candidate`, which is seeded from `s.desired`
(`config_store.go:188`), not from `s.live`. `copyAppliedKeys` (`config_store.go:244`)
then keeps `s.live` honest for only the *applied* keys - so the live seam and
`GET /config` deliberately disagree.

Auditor probe (executed):
- `PUT /config {"dataDir":"/new/","region":"eu-central-1"}` ->
  `200 {"applied":["region"],"restartRequired":["dataDir"]}`; `GET /config`
  reports the OLD `dataDir` while every S3 bucket resolves under `/new/`.
- `PUT /config {"buckets":{"b1":{"path":"/new/moved"}},...}` -> same shape.

This is the success-path twin of the very invariant `52b8e40` was written to
restore ("an invalid patch changes NOTHING"). The live/desired split the fix
introduced made the success path dishonest instead. `dataDir` alone does not
leak (`applied` empty -> `applyHotSeams` skipped); the co-patch is required.
No test pins it: `config_store_state_test.go:285` asserts `Snapshot()` only,
never the installed view.

Fix shape:
```go
if len(applied) > 0 {
	merged := deepCopyServerConfig(s.live)
	copyAppliedKeys(&merged, &candidate, applied)
	if err := applyHotSeams(&merged); err != nil {
		hotSeamsRollback(s.live)
		return nil, nil, err
	}
}
```
This also makes `hotSeamsRollback(s.live)` exact rather than approximate.

### H2. `PUT /config {"zfs_binary":...,"zfs_bucket_datasets":true}` executes a
### binary the API reported as restart-required.

`zfs_binary` is in `configUpdateKeys` (`config_store.go:59`) but NOT in
`hotApplyKeys` (`config_store.go:69-76`), so `classifyPatchKeys`
(`config_store.go:778`) reports it restart-required. But `applyHotSeams` ->
`installZfsDatasetProvisionerFor` (`s3_wiring.go:320-329`) captures
`cfg.ZfsBinary` into the live `zfsBucketCreate/Destroy/Exists` closures, which
`exec` it (`internal/frontend/s3/zfsdatasets.go:190-198`). Probed:
`applied=[zfs_bucket_datasets] restartRequired=[zfs_binary]`,
`GET /config zfs_binary` unchanged, yet the live provisioner installed the NEW
binary path. The reported "restart-required" value is the one running.

Related dead code: `copyAppliedKeys` has a `case "zfs_binary"`
(`config_store.go:255`) that is unreachable (`hotApplyKeys["zfs_binary"]` is
false, confirmed by probe).

Fix shape: make the hot install read the RUNNING `zfs_binary`
(`live.ZfsBinary`), and either drop the dead `copyAppliedKeys` case or add
`zfs_binary` to `hotApplyKeys` AND the seam installer so the report is honest.

### H3. DATA RACE: the SigV4 `region` global is written from an admin
### handler goroutine with no lock and read per request.

`internal/frontend/s3/region.go:26-31` carries the comment verbatim:
*"Not safe for concurrent use with in-flight requests: call at startup only."*
`config_store.go:213` now calls `SetRegion` from `applyHotSeams` inside
`PUT /config` - an admin handler goroutine - while every S3 request reads it
via `regionOf()`/`regionExplicit()` (`region.go:16,38`). `grep -n 'regionMu\|atomic'
internal/frontend/s3/region.go` = 0 hits: no lock exists. Every other hot seam
is mutex-guarded (`seam.go:43` `configViewMu`); `region` is the exception and
became hot in this wave.

**EXECUTED PROOF (parent, `-race`, overlay probe, one writer goroutine +
one reader goroutine through the real accessors):**
```
WARNING: DATA RACE
  internal/frontend/s3/region.go:30 +0xbc     <- SetRegion write
  internal/frontend/s3/region.go:17 +0xa8     <- regionOf read
FAIL github.com/bhodgens/zeta-object/internal/frontend/s3
```

Amplifier: a racy read can return a half-written string and answer a valid
`us-east-1` request with `SignatureDoesNotMatch`. This wave made the region a
hot, admin-reachable, per-request-read global.

Fix shape: `atomic.Pointer[string]` (or guard with the existing `hookMu`), and
correct the stale "call at startup only" comment.

---

## MEDIUM

- **M1.** `internal/frontend/s3/batch_bridge.go:50` - `bucketPath` parameter
  is DEAD (only occurrences are the signature and the doc comment), so the
  webdav/h3 `?batch` mount validates and authorizes against its OWN resolver
  but the executor reads/writes through s3's `getBucketPath`
  (`batch_endpoint.go:244,:327`). The doc comment's "production wires the SAME
  getBucketPath" is false for webdav in mode A. Fix: thread the resolved path
  onto `s3BatchExecutor` and prefer it.
- **M2.** `internal/frontend/webdav/copymove.go:165-168` - MOVE answers HTTP 500
  where DELETE answers 409 for the SAME error class
  (`ErrDeleteMarkersUnsupported` has no case in `errors.go:59` `davStatus`).
  Executed: `MOVE /snap.txt -> /moved.txt` on a versioning-Enabled bucket in
  snapshots mode -> 500; the same store state on `DELETE` -> 409. Fix: teach
  `davStatus` the two versioning sentinels (one place).
- **M3.** `internal/frontend/webdav/propfind.go:238` - PROPFIND still lists a
  delete-marked object. `ce7594a` added the consult only in `serveObject`
  (`get.go:70`); `propfindEntries`/`childEntries` build rows from
  `resolveKind` with no consult. Executed: PUT, DELETE (204+marker),
  `PROPFIND /` Depth 1 -> 207 with the key present while GET 404s. A syncing
  client re-downloads a key the server calls deleted.
- **M4.** `internal/frontend/s3/audit_log.go:133` and
  `internal/frontend/admin/routes.go:107` - both wrappers embed
  `http.ResponseWriter` with NO `Unwrap()`, so `http.ResponseController`
  cannot reach `Flush`/`Hijack`. `dispatch.go:111` wraps EVERY authorized s3
  request when the audit log is configured. Same defect `bf77bef` fixed in
  `altsvcWriter`, one layer in, so L6 is only half-closed. Fix: add
  `func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }`
  to both, plus a test. No test covers either wrapper's `Unwrap`.
- **M5.** `internal/frontend/s3/object_handlers.go:1757` - DeleteObjects still
  hard-deletes on a versioned bucket (M4 from the prior report, still open, and
  now with a second defect: `deleteBatchExecutor.Delete` discards the per-item
  context and `deleteObjectCore` hardcodes `context.Background()`
  (`object_handlers.go:746`), so a disconnected client does not stop the
  remaining 1000 keys on `?delete` while it does on `?batch`). No flag or
  config changes it; `DeleteRequestObj` has no `VersionId` field at all.
- **M6.** `internal/bucketmanager/bucketmanager.go:272-284` - `Env.Exists` is
  the unvalidated sibling: no `validateName` call, and its `//nolint:gosec // G703`
  comment is now an unbacked claim. EXECUTED: `Exists("../srvdata") = true`,
  `Exists("") = true`; through the real admin wiring, `GET /buckets/..` -> 200
  `{"name":"..","exists":true}`, plus an unvalidated `zfs exists` dataset probe
  via `adminBucketIsDataset` (`admin_wiring.go:353`). Information disclosure,
  not data loss.
- **M7.** `internal/bucketmanager/bucketmanager.go:216,244` - the rewritten
  `#nosec` comment claims "G703 is excluded repo-wide (.golangci.yml)". It is
  not: `.golangci.yml:81-107` excludes G104, G204, G304, G706, G301, G302,
  G306 and says G115 is deliberately NOT excluded. G703 is a live gosec v2
  taint rule. The fix replaced the old false claim with a second false claim.
- **M8.** `scripts/zfs-validate/run-zfs-validation.sh:2639-2643` - the harness
  reports green having verified nothing when no check ran:
  `results = []` at `:2154` -> `failed = []` -> `sys.exit(0)` ->
  `FIX_TOTAL: 0/0`. Same shape at `:1067/1254/1409/2018`. Parent-verified:
  `grep -cE 'len\(results\) *[<=]|assert results'` = **0 hits in 2676 lines**.
  b150a85's "52/52" is unfalsifiable. Fix: assert a minimum count per section.
- **M9.** `scripts/zfs-validate/run-zfs-validation.sh:2657` -
  `if [[ $RC -ne 0 || $KEEP_SERVER -eq 0 ]]` - section 15's teardown
  (`:2092-2103`) kills the pid BEFORE the flag is consulted
  (`grep -n KEEP_SERVER` = only 58, 59, 2657), and `$RC -ne 0 ||`
  short-circuits so a FAILING run destroys the dataset the operator wanted to
  poke at, immediately before `:2673` prints "Server log follows."
  Contradicts the docstring at `:30`.
- **M10.** `scripts/zfs-validate/run-zfs-validation.sh:2625-2632` -
  `except Exception: coll_events = []` then `assert coll_events == []`: any
  decode error passes. The neighbouring blocks (`:2454-2459`, `:2480-2486`)
  fail closed; this one does not. Also 7 bare `wd(` calls discard status, and
  `:2608` "COPY recorded NO version" PASSES if both source PUTs failed.
- **M11.** `client_ca_reload_test.go:266` is the ONLY `resetClientCAReloaders()`
  call in the package while `frontends.go:158` auto-registers on every frontend
  construction. Reproduced 3/3:
  `go test . -count=1 -shuffle=4 -run 'QUIC|ReloadAuthReloadsIdentitiesAndCA'`
  -> `FAIL: ReloadAuth: h3 client CA reload: .../ca.pem: no such file or
  directory`. A leaked h3 registrant outlives its `t.TempDir()`. CI green is
  ordering luck. Fix: `t.Cleanup(resetClientCAReloaders)` in the shared
  frontend-construction helpers, not per-test.
- **M12.** `config_store_state_test.go:70` never sets `zfsBucketsParentDataset`
  and is the only one of four M7 tests lacking the save/restore;
  `zfs_wiring_test.go:30` sets it with no restore. Reproduced:
  `go test . -shuffle=6 -run 'TestInstallS3SeamsZfsBucketDatasetsOn|TestConfigStoreRejectedPatchLeavesLiveRegionUnchanged'`
  -> FAIL; without `-shuffle` -> ok.

---

## LOW (selected - full list in the auditor transcripts)

- **L1.** `config_store.go:278` `hotSeamsRollback` - the named invariant holder
  has ZERO executions: parent-measured coverage `config_store.go:279.2,279.44 1 0`.
  Delete it, or add any fallible step before the provisioner install, and the
  suite stays green while a rejected `PUT /config` leaves the live region
  mutated. `TestConfigStoreRejectedPatchRollsBackHotSeams` passes only because
  `validateHotSeams` short-circuits first - its own comment claims otherwise.
  Fix: an injectable failing installer seam so the rollback actually runs.
- **L2.** `internal/bucketmanager/bucketmanager.go:159` and `delete.go:23` -
  the guards check `e.BucketPath == nil || e.Locks == nil` but not
  `e.Custom == nil`, which `validateName` calls (`validate.go:131`).
  EXECUTED: `PANIC from validateName with nil Custom`. Both production
  `Install` sites set `Custom`, so not reachable today - a new panic surface.
- **L3.** `internal/frontend/webdav/dispatch.go:172-180` still carries its own
  private copy of the credential-rejection predicate that `b5223f3` extracted
  into `internal/frontend/autherr`; only `owncloud.go:290` delegates
  (`grep -rn autherr internal/frontend/webdav/` = 0 hits), and the comment at
  `:169-170` still claims it is "the single place that decides 401-vs-500".
- **L4.** `internal/frontend/s3/export_versioning_test_surface.go` has no build
  constraint and is in `.GoFiles`; `ValidateObjectKey` is defined ONLY there
  (`export_test_surface.go:140`) and is consumed by PRODUCTION webdav code
  (`webdav/batch.go:66`). So the "test surface" is now load-bearing production
  API, and adding a build constraint breaks the webdav batch mount. Also
  `SetRegionForTest` (`export_test_surface.go:114`) writes the UNLOCKED region
  global. Fix: move `ValidateObjectKey` into a real production file first.
- **L5.** `internal/frontend/autherr` identity fallback: `s3/dispatch.go:197`
  and `batch_endpoint.go:129` fall back to `auth.WildcardIdentity("unauthenticated")`
  - a full `{"*": {read,write}}` grant - so a future frontend that forgets
  `WithIdentity` gets wildcard authority silently instead of denying.
  Unreachable on the wire today (`authenticateRequest` runs first).
- **L6.** `config.go:484` - `ServerConfig.UnmarshalJSON`'s alias is a second
  hand-maintained field mirror of the struct's JSON tags; the drift L8 found
  lived in the OTHER mirror (`bucketCfg.UnmarshalJSON`, now closed). Fix: a
  reflection test over the JSON tags, the `validate_sync_test.go` idiom.
- **L7.** `internal/frontend/s3/batch_endpoint.go:223-242` - `7a8ad1b` moved the
  If-Match check AFTER `io.ReadAll`, so a stale-ETag manifest buffers every
  source object before rejecting, unbounded (`handleBucketBatch` caps the
  manifest at 4 MiB, nothing caps the copied bytes). The old code rejected
  without buffering. Fix: `io.LimitReader(rc, maxPutBytes+1)`, or Stat-then-Get.
- **L8.** `internal/frontend/s3/batch_endpoint.go:218-308` - batch copy writes
  no `after_upload`/`after_delete` action trigger (7 trigger sites exist across
  the single-op handlers), so per-bucket `.bucket-actions` hooks skip every
  batch write. Parity with CopyObject holds on metadata/tags/principal, not on
  actions.
- **L9.** `internal/frontend/s3/object_handlers.go:613-641` -
  `serveObjectRangeFrom` never returns false under its guard, so the GET reopen
  block is dead code; if it ever became reachable, `defer srcRC.Close()` binds
  the RECEIVER, so after `srcRC = srcRC2` the deferred close would close the old
  reader and `srcRC2` would leak - the same fd class the wave just fixed, one
  line away.
- **L10.** `internal/frontend/s3/reflinkversions.go:46-50` opts `reflinkCloneFn`
  out of `hookMu`; the write is in `export_versioning_test_surface.go:92-94`
  (a production-compiled file) and the read at `:190` is reachable from two
  parallel handlers. Latent, fixed for free by L4.
- **L11.** `internal/frontend/webdav/copymove.go:131,165` - the MOVE gate is
  duplicated; the tail's `if isMove` is dead-guarded. Correct today, noted so
  the next edit does not reintroduce an ungated source delete.
- **L12.** `admin_traversal_test.go:76` asserts `parent/etc` is absent, but no
  name in `adminTraversalNames` resolves to it; the one in-tree escape the
  commit names (`a/b` -> `dataDir/a/b`) is never asserted.
- **L13.** `client_ca_reload_test.go:478` - a wave test has a `-race` data race
  (`config.go:468 loadConfig` writes a process global while a leaked h3
  goroutine reads it in `bucketPathFor`, `h3/serve.go:48`). Latent only because
  `-race` is not in the default gate.
- **L14.** `validateHotSeams` (`config_store.go:733-745`) installs then
  UNINSTALLS the provisioner and clears the dataset parent as its "side-effect
  free" probe; it unconditionally destroys whatever was live. Benign today
  only by ordering coincidence.

---

## Claim-verification verdicts (the wave's own statements)

| Claim | Verdict |
|---|---|
| b433eb1 "four tests assert `bucketPathFn == nil`" | **HOLDS** - exactly 4 `newProdWiredVerEnv` consumers; `versioning_gate_test.go:56-58` fails if non-nil. |
| b433eb1 "fail-CLOSED 500 in all three gates" | **FALSE (2/3)** - `batch_endpoint.go:255` `versionStoreForBucket(...).State()` error path has no test. |
| b433eb1 H1 closed | **HOLDS** - the 4 production-wired tests fail on revert of `delete.go:52` alone. |
| ce7594a H2 closed | **PARTIAL** - GET/HEAD closed, PROPFIND not (M3). |
| 7a8ad1b H4 fd leak closed | **HOLDS** - one Get, one close at `batch_endpoint.go:230` covering every return path; tracking backend asserts `unclosed == 0`. |
| 7a8ad1b M1 parity closed | **HOLDS** - byte-identical `customMetadata`, `contentType`, `tags`, owner/writer breadcrumbs. |
| 7a8ad1b M3 bucket gate closed | **HOLDS on `?batch` only**; the sibling surfaces are C1. |
| 52b8e40 "the rollback is the invariant holder" | **UNPINSTED** (L1) - zero executions. |
| 52b8e40 M7/M8 closed | **HOLDS for the rejection path** - every mutated global restored, probed. The SUCCESS path has H1/H2/H3. |
| c0df3a8 L5 closed | **HOLDS** - both registrants through one registry, load-then-store, fail-closed on error. |
| 87dd8fc L3 closed | **HOLDS** - both arms, plus the retention-0 harm with an armed control. |
| b150a85 "52/52 section-15 checks" | **UNFALSIFIABLE** (M8) - an empty result list reports `0/0` and exits 0. |
| b150a85 "219 PASS / 0 FAIL (S3 90, ZBD 33, H3 27, s15 52)" | **NOT REPRODUCIBLE** - the breakdown sums to 202; the missing 17 reconcile to section 13, silently omitted. |
| b150a85 "no assert weakened" | **LITERALLY TRUE** (1 removed line, a comment) but the same commit ADDED guard-conditional checks - weakening by omission. |
| README + protocol-compatibility.md reload claims (c0df3a8) | **HOLDS** - both verified against `reloadRegisteredClientCAs`. |

---

## AGENTS.md rule compliance for this wave

- **E2E coverage rule (violated).** `git diff --stat b433eb1~1..HEAD --
  scripts/e2e/` is EMPTY: zero e2e cases across 12 commits, while the wave
  changed user-facing surface (`PUT /config` contract, `POST /auth/reload`
  rotating two listeners, webdav `?events` on collections). Case 36 calls
  `/api/auth/reload` but was not extended to assert the multi-listener fan.
- **ZFS validation rule.** `scripts/zfs-validate/run-zfs-validation.sh` exists
  and the wave extended it (629 lines), but M8/M9/M10 mean its green is not
  evidence until the count floor lands.
- **Coverage floors.** No code moved between packages, so `COVER_MIN 47` does
  not need re-measuring this wave.

---

## Untracked / doc state (`git check-ignore -v`: none of the six are ignored)

| Path | Disposition |
|---|---|
| `.claude/mind.mv2` (4.0 MB binary) + `.lock` | **gitignore + delete** - local agent scratch DB. |
| `.claude/settings.local.json` | **gitignore** - the name is the convention. |
| `scripts/e2e/__pycache__/` | **gitignore** - never tracked (`git ls-files` = 0). |
| `testdata/fuzz/FuzzGetCanonicalURI/5763d8e1c47c4a7a` | **Commit** - a fuzz corpus seed; precedent is tracked. |
| `docs/plans/zfs-bucket-datasets-2026-10/` (6 files) | **Commit** - 22 tracked plan trees; AGENTS.md mandates the layout. |
| `scripts/e2e/run-one.sh` | **Commit or delete** - no Makefile/CI reference (0 hits). |
| `.hermes/audits/2026-10-05-last-week-bughunt.md` | **Commit** - 4 `.hermes` files already tracked. |

---

## Prior-report staleness

The wave closed H1-H5, M1, M3, M6-M8, L1-L9. Still open and still correctly
listed in the prior report: **M2** (batch move half-success), **M4**
(`?delete` hard-deletes on a versioned bucket - now M5 above), **M5** (test
surface in the binary - now L4), **L10** (admin console CSRF / session sweeper
/ login throttle). The wave did not over-claim: no fix is asserted for
M2/M4/M5/L10. Its coverage-gap list is partly stale - the unknown-CN case IS
covered (`webdav/dispatch_certauth_test.go:111`); no expired-cert case exists.

---

## Disclosure ledger - what this audit did NOT verify

1. **No live ZFS run.** `zfs-meta` was never contacted; every harness claim
   (b150a85's 219 PASS, section 15's 52/52) is UNVERIFIED here. The static
   findings M8/M9/M10 stand on their own; the pass counts do not.
2. **No e2e case was written or extended** to pin C1, so C1 has no automated
   proof in-tree - the parent's overlay probe is the only evidence.
3. **No `-race` gate run over the full suite**, only the targeted region probe
   and the auditor's two test-file races. L13's sibling races may exist.
4. **`-shuffle` was not run over the whole module**; M11/M12 reproduce on the
   named subsets only.
5. **The auditors' own HIGHs were not all parent-verified.** Parent-verified by
   execution: C1, H1, H2, H3, M2, M3, M6, L2. Read-verified only: M1, L1, L4,
   L7, L8, L9, L10. The batch-race (`?delete` context) claim is read-only.
6. **Four child subagents were spawned by the auditors** and their outputs are
   folded into the parents' reports; one child claim (a dropped `caErr` at
   `admin_wiring.go:208`) was REFUTED on re-read - the error is checked and
   returned.
7. **`internal/frontend/admin/` session/CSRF items (prior L10) were not
   re-audited** this wave.
8. **Auditor line numbers drift.** Every CRITICAL/HIGH above was re-located by
   grep and re-verified by execution, not by trusting the reported line.
9. **The tree is byte-identical to how it was found**: `git diff --stat`
   empty; all probes ran via `go test -overlay` against `/tmp` and were
   deleted.
10. **No fix was applied.** Report-only; the wave's 12 commits stand unamended.

---

## Next steps, ranked

1. Fix C1: one gate in `objectLevelDispatch` plus four bucket-level gates;
   add `scripts/e2e/cases/39-bucket-name-gate.sh`.
2. Fix H3: make `region` atomic; it is a one-file change with a `-race` proof.
3. Fix H1 + H2 together: build the installed view from `live` + applied keys,
   and stop the provisioner install from reading a restart-required binary.
4. Land the harness count floor (M8) and the `--keep-server` ordering (M9);
   until then no "52/52" is evidence.
5. `t.Cleanup(resetClientCAReloaders)` in the shared frontend helpers (M11).
6. Extend e2e case 36 to assert both listeners rotate.