# Wave-2 D: fix-efficacy + defect audit — webdav / admin server / auth

Repo: /Users/caimlas/git/mini-s3  HEAD=7f4a544 (16 unpushed commits)
Scope: H3, H9, H10, M13, M15, M16, M31, L50. Fix commits 4dadf9c, 534074e, abf547e, 1d471ca(F1).
Read-only: no tracked file modified.

## 1. Fix-efficacy per finding (code at HEAD, not the diff)

### H3 — webdav `POST ?batch` authorized as READ -> read-only identity can write/delete
VERDICT: CLOSED.
Path traced end to end:
- `dispatch.go:48` `authorize(...)` classifies POST via `requestGrantsWrite("POST")` -> `default: return false` (dispatch.go:259-260) => READ gate; a read-only identity passes here.
- `dispatch.go:58` calls `batchDispatch` BEFORE the method switch.
- `batch.go:79-82` (added by 4dadf9c) `frontend.AuthorizeRequest(identityOf(r), res.bucket, true)` -> `authz.go:47-58` -> `auth.OuthorizeOp(id, auth.OpWrite, bucket, "", now)`.
- read-only identity: `richgrants.go:358-371` / `authorizeV1Op` -> `Identity.CanWrite` (grants.go:22-28) = false => 403 written, returns true before `s3.HandleBatchForBucket`.
=> A read-only identity CANNOT reach a manifest delete. Verified independently that `HandleBatchForBucket`/`s3BatchExecutor` and the `?delete` `deleteBatchExecutor` perform NO per-item grant check (batch_bridge.go:72-114, batch_endpoint.go:79-117, object_handlers.go:1808-1892), so the gate in batch.go:79 is the only one on that path.
Test: `internal/frontend/webdav/batch_test.go:189 TestWebdavBatch_ReadOnlyIdentityIs403` uses a REAL `auth.NewMultiRegistry` read-only grant (`rawGrants(...{"ro-batch-bkt":"readonly"})`), NOT `auth.WildcardIdentity` -> does NOT hide the defect. It would FAIL pre-fix: `git show 4dadf9c^:internal/frontend/webdav/batch.go` has no grant check between the `bucketPath == ""` seam and `s3.HandleBatchForBucket`, so the pre-fix response is 200 with victim.txt deleted (assert `w.Code != 403` fires). e2e part 19h (`scripts/e2e/cases/19-webdav.sh:487-499`) drives the same shape with a `readonly` grant and a read-write positive control.
NOTE: the sibling batch tests (`TestWebdavBatch_CopyMoveDelete`, `_Malformed400NothingExecutes`, `_GETIs405`) run behind `newStubAuth()` which is `auth.WildcardIdentity("stub")` (stubauth_test.go:18-20) — they cannot see any auth regression. That is expected for behavior tests; the auth pin is the real-registry one.

### H9 — webdav MOVE Destination == request URI deletes the object, answers 204
VERDICT: CLOSED.
`copymove.go:61-64` refuses `dstRes.bucket == src.bucket && dstRes.key == src.key` with 403 BEFORE `resolveKind`/`Get`/`Put`/`Delete` (the guard sits between the destination-write gate at :44 and the first backend call at :65). RFC 4918 9.9.4 wants 403; the guard also correctly covers COPY (same-URI copy is also 403 per 9.8.5).
Test: `copymove_conflict_test.go:83 TestWebdavMOVE_SelfDestinationIs403` asserts 403 + bytes intact + no delete marker, in both sidecar and snapshots modes; `:125 TestWebdavMOVE_SelfDestinationAcrossModes` pins bare-path and full-URL dest forms plus the no-overfire case (a DIFFERENT key still moves 201). Both FAIL pre-fix (`git show 4dadf9c^:internal/frontend/webdav/copymove.go` has no guard: pre-fix the flow Puts then Deletes and answers 204).

### H10 — webdav recursive collection DELETE bypasses the versioning delete-marker path
VERDICT: CLOSED.
`delete.go:101-113` `deleteCollectionKey` runs `deleteMarkerOrPlain` (versioning.go:120-122 -> s3 `DeleteObjectVersionedMarkerForBucket`) and SUPPRESSES the plain delete on an Enabled bucket; the single-key branch (`:51`) and the collection sweep (`:170`) both call it, so there is one path. Snapshots-mode refusal propagates and maps to 409 (`:52-56` and `:74-77`). Sweep convergence uses `objectHiddenByDeleteMarker` (`:178`, markerhide.go:55-70) so a marker-suppressed key does not spin the `maxSweeps` loop.
Test: `delete_versioning_test.go:43` (Enabled: one marker per key, data file survives, plain views hidden), `:89` (snapshots: 409 and the tree survives), `:117` (unversioned still destroys — the "don't fix by refusing" guard). Pre-fix `deleteCollection` called `f.be.Delete` per key directly -> the `:43` test's marker assertions FAIL pre-fix.

### M13 — rich grant create-but-not-write/delete authorizes `POST /{bucket}?batch`
VERDICT: CLOSED (stated defect), with a residual that is reported as NEW finding #1.
`s3/dispatch.go:248-264` `opForMethod`: `POST` with empty key and `?batch` -> `OpWrite` (`:255-257`), `?delete` -> `OpDelete` (`:258-260`), plain bucket-root POST (CreateBucket) keeps `OpCreate` (`:262`). A create-only rich entry therefore no longer authorizes `?batch`. Test `internal/frontend/s3/bughunt_m13_test.go:36` uses `{"ops":["create"]}` and asserts 403 on `?batch` and `?delete` while PUT /bucket still 200s; FAILS pre-fix (`git show 1d471ca^:internal/frontend/s3/dispatch.go`).
RESIDUAL (not the stated defect): the batch manifest executes deletes, and `OpWrite` is the only gate, so a WRITE-ONLY grant still deletes through `?batch` on both the s3 and webdav mounts. See finding #1.

### M15 — `/logout` state-changing POST outside the session/CSRF gate
VERDICT: CLOSED.
`server.go:300-317`: `handleLogout` now runs `sessionGate`'s two checks inline — `sessions.validate(r)` (401 when no live session) then `tokenEqual(r.Header.Get(CSRFHeaderName), csrf)` (403 on missing/wrong), then `clear`. Neither rejection touches the session map. `Handler()` (`:150-151`) still routes `/logout` outside `sessionGate`, which is why the checks are duplicated rather than shared — a maintenance smell, not a defect.
Test: `server_test.go TestLogoutRequiresSessionAndCSRF` (401 no-session, 403 no-CSRF, 403 wrong-CSRF, session still alive after each, 204 with both). FAILS pre-fix (`git show 534074e^:internal/adminserver/server.go`: no checks -> 204 for every POST). UI compatibility verified: `internal/adminserver/web/assets/app.js:458-459` sends `X-CSRF-Token` on `/logout`, so the added gate does not break the console.

### M16 — cookie `Secure` derived from `r.TLS` -> TLS-terminating proxy mints non-Secure cookies
VERDICT: CLOSED (opt-in; residual reported as NEW finding #6).
`session.go:141` and `:218` now use `s.cfg.SecureCookies(r)`; `config.go:147-152` returns `r.TLS != nil || c.TrustProxyTLS` (nil-cfg safe). The stated defect — derivation from `r.TLS` ALONE — is gone. Test `session_test.go TestCookieSecureFollowsTransportAndConfig` covers plain-no-knob (non-Secure), TLS (Secure), trustProxyTLS-on-plain (Secure, incl. the logout expiry path) plus a nil-config pin; the trustProxyTLS arm cannot compile/fail meaningfully pre-fix (the field did not exist), and the plain/TLS arms pin the unchanged behavior.

### M31 — lock file names collide across distinct keys
VERDICT: CLOSED.
`lockstore.go:66-68` `lockFileName` = `url.PathEscape(strings.TrimPrefix(key,"/")) + ".lock"`. `PathEscape` escapes `/` (=> no path structure) and `%` (so the mapping is unambiguous/injective); pre-fix `strings.ReplaceAll(.., "/", "_")` mapped `/a/b_c.txt` and `/a/b/c.txt` onto one name.
Tests: `lockstore_test.go:248 TestFileLockStore_LockFileNameInjective` (collision set incl. the historic pair; FAILS pre-fix on the dup detector) and `:270 TestFileLockStore_DistinctNestedKeysDoNotConflict` (two distinct on-disk lock files, both Acquires succeed). Both fail pre-fix.

### L50 — doc drift
VERDICT: OPEN (second half already correct).
(a) STILL OPEN: `internal/frontend/s3/object_handlers.go:1894-1899` — the `deleteBatchExecutor` type comment still reads "...the versioned-marker dispatch the DELETE handler runs is NOT taken here — DeleteObjects on a versioned bucket removes the data outright, the pre-leaf behavior this wire has always had". The code at `:1929-1938` (`Delete`) takes `deleteObjectVersionedMarker` FIRST and suppresses the plain delete — the exact opposite. Contradicted by `coverage_restore3_test.go:143`. Not touched by any of the 11 fix commits.
(b) ALREADY CORRECT at HEAD: `scripts/e2e/e2e-cleanup.sh:105-112` describes the ORPHAN (PPID 1) gate and explicitly records that it replaced the "no LISTENing socket" gate; `AGENTS.md` item 4 matches (orphan rule). The report's L50 complaint about AGENTS.md no longer holds.

### abf547e (OCFileID determinism pin)
`propfind_test.go` now computes both sides through `wrapOCFileID`. The test CAN still fail (it fails if `OCFileID` is non-deterministic), so it is not a dead test; it wires no identity and hides nothing.

## 2. NEW defects (touched code)

1. MEDIUM — batch sub-resources gated on OpWrite alone although the manifest executes deletes.
   `s3/dispatch.go:255-257` (`?batch` -> OpWrite) + `webdav/batch.go:79` (`AuthorizeRequest(..., true)` -> OpWrite). No per-item grant check exists anywhere in the batch pipeline. A rich grant `{"ops":["write"]}` (no delete) authorizes `POST ?batch {"operations":[{"op":"delete","from":"victim"}]}` on both mounts and the delete executes, while the single-op DELETE path correctly requires OpDelete (`?delete` -> OpDelete, `s3/dispatch.go:258-260`). Privilege escalation beyond the grant; also a drift against the `?delete` classification.
2. MEDIUM — authorization predicate drift in the webdav adapter.
   `webdav/dispatch.go:251-262` (`requestGrantsWrite`) collapses every verb to read/write and never yields OpCreate/OpDelete; `authorize` then calls `frontend.AuthorizeRequest(..., write bool)` (authz.go:47-58) which maps only to OpRead/OpWrite. Consequences: a rich grant `{"ops":["delete"]}` (or `["create"]`) is INERT on the webdav mount (DELETE/MKCOL 403) while the same grant works on s3; and a `{"ops":["write"]}` grant can DELETE/MKCOL on webdav. Two wire protocols rendering the same grant vocabulary with two different predicate tables.
3. MEDIUM — webdav `POST ?batch` bypasses WebDAV write locks.
   `webdav/dispatch.go:58` runs `batchDispatch` BEFORE the method switch, and the lock gate (`enforceWriteLock`) lives only in the PUT/DELETE/COPY/MOVE cases (`:84-98`); `HandleBatchForBucket` never consults the lock store. Trigger: `LOCK /b/f.txt` (200) then `POST /b/?batch {"op":"delete","from":"f.txt"}` -> 200 and the locked file is removed. Effect: a lock the client believes protects the resource is bypassed; data loss on a locked resource.
4. LOW — LOCK/UNLOCK classify as READ.
   `webdav/dispatch.go:251-262`: neither verb has a case, so both fall to `default: return false` and the gate is `AuthorizeRequest(id, bucket, false)` = OpRead. A read-only identity can `LOCK` every key it can read (writes server state under `.metadata/.locks/` and answers 423 to every writer). Effect: grant-escalation/DoS by a read principal; also writes server-owned state the charter wants minimized.
5. LOW — L50 doc drift still open (see above), proof `object_handlers.go:1896-1898` vs `:1930-1938`.
6. LOW — M16 residual: opt-in only. `config.go:56-62` defaults `TrustProxyTLS=false` and nothing validates/warns, so the default deployment behind a TLS-terminating proxy still mints non-Secure session/CSRF cookies; `SecureCookies` ignores `X-Forwarded-Proto`.
7. LOW — e2e temp-file hygiene regression in the same file 4dadf9c touched.
   `scripts/e2e/cases/19-webdav.sh:490` and `:507` `mktemp /tmp/e2e19-ro.XXXXXX`; `w19_cleanup()` (`:78-88`, trap at `:90`) removes only `W19_ROOT/W19_WORK/W19_CERT/W19_FAILOG` — `W19_RO_BODY` is removed only by the inline `rm -f` at `:499`/`:512`. This violates the AGENTS.md "Cleanup is a harness obligation" rule (the case-32/33 shape); an assert/`set -e` abort between mktemp and rm leaks the file (the age-gated sweeper namespace `/tmp/e2e[0-9]*` catches it only on the next run).

## 3. Gates
- `go test -count=1 ./internal/frontend/webdav/... ./internal/adminserver/... ./internal/auth/...` -> ok (webdav 5.189s, adminserver 0.537s, adminserver/gateway 1.271s, auth 1.401s); 0 failures.
- `go vet ./...` -> exit 0, no diagnostics.
- `make e2e` NOT run (per instruction).

## 4. Disclosure
- No live ZFS run; the versioning-marker assertions are read from code + unit tests.
- Pre-fix failure determinations are by reading `git show <hash>^:<path>` (no checkout/stash used).
