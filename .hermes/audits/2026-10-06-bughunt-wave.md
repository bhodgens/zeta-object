# Bughunt audit - wave b150a85..269c193 (2026-10-06)

Scope: the 17 commits landed 2026-10-05/06 (plus docs commit `a1b971a`, verified to touch 0 Go files) on `main` in `/Users/caimlas/git/mini-s3`
(the fix campaign that closed `.hermes/audits/2026-10-05-last-week-bughunt.md`,
plus the follow-on fixes and one new feature). Baseline verified GREEN in the
parent before any finding: `go test ./... -count=1 -p 4` = 21 packages ok,
0 FAIL (`/tmp/bughunt_baseline_unit.log`), re-run after the mid-wave commit
landed (`/tmp/bughunt_baseline_head.log`, same result). So every finding below is
new-introduced or latent, not baseline noise.

Every finding in this report was parent-verified BY EXECUTION (a throwaway
`go test -overlay` probe living in `/tmp`), not by reading alone. The repo was
not modified by any probe; `git status` is unchanged (3 untracked scratch items).

## AUDIT METHOD - DEVIATION DISCLOSURE

The intended method was 7 parallel read-only auditors over disjoint scopes. All
7 died within 1.4s each on `HTTP 400: Upstream request failed: Model is
unavailable` (provider outage, not a repo fault), plus one re-dispatch of the
webdav scope which died the same way in 0.72s. The consolidated batch report
later confirmed all 7 as `status=failed` with `api_calls=1` each - i.e. each
child failed on its FIRST model call and did no work at all.

**Zero auditor output exists.** This report is therefore PARENT-DIRECT ONLY:
narrower coverage, no independent second opinion, and no per-scope revert-matrix
sweep except where the parent ran it by hand. The parent DID run every scope:
the gate honesty pass, the cross-cutting sweep, the harness/bucketmanager/rename
sweep, the bucket-name gate sweep, and the webdav + s3 findings during
pre-reads. Anything the dead auditors would have covered and the parent did not
reach is listed in the disclosure ledger, not silently omitted.

## Verdict

**READ THIS FIRST: the repository's declared validation gate can report green
having verified nothing.** `549d238` added a check-count floor helper to
`scripts/zfs-validate/run-zfs-validation.sh` and never called it. Every finding
below is subordinate to that. Details in Y1.

The wave closed the C1 traversal hole completely (every bucket-name surface is
gated, verified by enumeration) and bucketmanager is clean (Y2). It also left the
validation harness unfalsifiable, one HIGH data race, one HIGH four-way
configuration-authority split, one MEDIUM delete-semantics asymmetry on the S3
batch delete surface, two MEDIUM WebDAV collection-token defects in the new
feature, and one MEDIUM order-dependence regression with proven provenance.

| Severity | Count | New this wave | Pre-existing |
|---|---|---|---|
| HIGH | 4 | 4 | 0 |
| MEDIUM | 4 | 4 | 0 |
| LOW | 4 | 4 | 0 |
| MEDIUM (upgraded L1) | 1 | 1 | 0 |
| LOW (Y3) | 1 | 1 | 0 |

Plus 4 INFO items from the gate-honesty pass (G2-G5) and one added LOW (L3).

---

## HIGH

### H1. DATA RACE: the SigV4 `region` global is written from an admin handler
### goroutine and read per request, with no lock.

`internal/frontend/s3/region.go:30`:
```go
func SetRegion(r string) {
	region = strings.ToLower(r)
}
```
`internal/frontend/s3/region.go:17`:
```go
func regionOf() string {
	if region == "" {
```
`grep -n 'Mu\|atomic\|Lock\|sync' internal/frontend/s3/region.go` = **0 hits**.

`SetRegion` is called from `applyHotSeams` (`s3_wiring.go:210`), which
`config_store.go:213` invokes from `PUT /config` - an admin handler goroutine.
`regionOf()`/`regionExplicit()` run once per S3 request.

**EXECUTED PROOF (parent, `/tmp` overlay, one writer + one reader through the
real accessors, `go test -race`):**
```
WARNING: DATA RACE
Read at 0x0001035dcfd0 by goroutine 34:
  internal/frontend/s3.regionOf()   .../region.go:17 +0xa8
Previous write at 0x0001035dcfd0 by goroutine 33:
  internal/frontend/s3.SetRegion()  .../region.go:30 +0xbc
FAIL github.com/bhodgens/zeta-object/internal/frontend/s3
```
Amplifier: a torn string read can answer a valid `us-east-1` request with
`SignatureDoesNotMatch`. The comment at `region.go:23` still says "call at
startup only", which the hot-apply path made false in an earlier wave.

Fix shape: `atomic.Pointer[string]` (or the package's existing `hookMu`),
and correct the stale comment.

### H2. ONE config patch leaves FOUR authorities disagreeing about where the
### data lives. `GET /config`, the installed view, the fs-root resolver and
### the backend lookup each answer differently.

Authorities, all reachable in one process:
1. `serverConfig()` - the atomic pointer (`config.go:366`), written by
   `setServerConfig` on every reload.
2. the installed s3 config view (`internal/frontend/s3/seam.go:68`), rewritten
   by `applyHotSeams` (`s3_wiring.go:196`) from the CANDIDATE config.
3. the `fsRootResolver` closure installed ONCE at startup against the startup
   snapshot (`s3_wiring.go:77`, fed by `main.go:175`
   `installS3Seams(&startupConfig)`).
4. the backend lookup built ONCE by `initBackendLookup` (`backend_lookup.go:180`),
   which is what actually reads and writes bytes.

`applyHotSeams` updates (2) and the region, but never rebuilds (3) or (4).

**EXECUTED PROOF 1 (parent, `main` package, `/tmp` overlay):** after a hot patch
that moves `dataDir` and carries a hot key:
```
AFTER HOT PATCH backend root            = ".../001/startup"
AFTER HOT PATCH serverConfig().DataDir  = ".../001/new"
```
**EXECUTED PROOF 2 (parent, s3 package):** after `applyHotSeams` with a new
`DataDir`:
```
AFTER hot patch: view DataDir="/CANDIDATE/new"  resolver path="/STARTUP/data/b1"
```
For `dataDir` specifically this is CORRECT (the key is restart-required, and the
data plane must not move without a restart). The finding is that authorities (1),
(2) and (3) now disagree, which is the precondition for H3.

### H3. `validBucket` authorizes one bucket path while the data plane uses
### another. The gate and the resolver read two different config generations.

`internal/frontend/s3/bucket_handlers.go:42`:
```go
func validBucket(name string) bool {
	if _, isCustom := currentServerConfig().Buckets[name]; isCustom {
		return true
	}
```
`currentServerConfig()` is the HOT-installed view (H2 authority 2).
`getBucketPath` (`bucket_handlers.go:26`) resolves through the STARTUP-frozen
`fsRootResolver` (authority 3).

**EXECUTED PROOF (parent, s3 package):**
```
validBucket("photos") = true   (gate reads the CANDIDATE custom map)
getBucketPath("photos") = "/STARTUP/data/photos"  (path the data plane uses)
DIVERGENCE: the gate authorizes photos as a custom bucket under /mnt/fast/photos
but the resolver writes it to "/STARTUP/data/photos"
```
Request shape: `PUT /config {"bucketSettings":{"photos":{"path":"/mnt/fast/photos"}}}`
co-patched with any hot key, then any s3 object request on `photos`. The gate
says "declared custom bucket, exempt from naming rules"; the resolver ignores the
new mapping entirely.

Fix shape: make `getBucketPath` read the installed view (authority 2) instead of
the startup-frozen closure, OR stop hot-installing a view the resolver cannot see.
One authority, one reader.

---

## MEDIUM

### M1. `POST /{bucket}?delete` HARD-DELETES on a versioning-Enabled bucket
### while the single `DELETE` writes a recoverable marker. One bucket, one key,
### two delete semantics, one of them unrecoverable.

`internal/frontend/s3/object_handlers.go:1856`:
```go
func (e deleteBatchExecutor) Delete(_ context.Context, key, ifMatch string) error {
	if err := deleteObjectCore(getBucketPath(e.bucket), e.bucket, key); err != nil {
```
It calls `deleteObjectCore` directly. The single-object handler calls
`deleteObjectVersionedMarker` FIRST (`object_handlers.go:714`) and suppresses the
plain delete when the bucket is Enabled. The JSON `?batch` surface DOES get this
right - `batchDelete` (`batch_endpoint.go:430`) calls the marker step. So the S3
XML batch surface is the outlier.

**EXECUTED PROOF (parent, s3 package, versioning Enabled through the real
`PUT /{bucket}?versioning` handler):**
```
single DELETE: "Delete marker written for "bkt1"/"victim.txt" (versioning enabled)"  status=204
?delete:       "Successfully deleted object "bkt1"/"victim.txt"  status=200
?delete arm:   version List error = NoSuchKey ; 0 version(s) recorded
ASYMMETRY: single DELETE recorded a marker but POST ?delete recorded none
```
Also carries the prior report's context defect: `deleteObjectCore`
(`object_handlers.go:746`) hardcodes `context.Background()`, so a disconnected
client does not stop the remaining 1000 keys on `?delete`.

Fix shape: give `deleteBatchExecutor.Delete` the same marker-first order as
`deleteObjectHandler`, and thread the request context into `deleteObjectCore`.

### M2. The new WebDAV collection change token does NOT move when a child is
### tombstoned. The feature's whole purpose is to tell a syncing client that
### something changed, and a deletion is the case it misses.

`internal/frontend/webdav/colltoken.go` derives the token from the PROPFIND child
ROWS (`tokenFromChildren`). Rows for delete-marked keys are dropped by the marker
consult (`propfind.go:248`), so a tombstone changes the row set - but the
executed result shows the token is unchanged.

**EXECUTED PROOF (parent, webdav package, production-wired env from
`propfind_marker_test.go`, versioning Enabled, two children):**
```
collection /2024/ token BEFORE delete = "dir-611feda296f6e125"
collection /2024/ token AFTER marker  = "dir-611feda296f6e125"
gone.txt still advertised = false
```
Client-observable symptom: an ownCloud/Finder client that recorded
`dir-611feda296f6e125` for `/2024/`, then sees the same token after a deletion,
never re-walks the directory and never learns the key is gone. (This is the same
class the wave's own `857df74` fixed for the LISTING; the token feature inherited
the assumption that row-set changes always move the token, which is only true
when the change is a write.)

### M3. The Depth-1 root collection's token is written to a local copy and lost.

`internal/frontend/webdav/propfind.go:296-303`:
```go
out := []propfindEntry{root}
if depth1 {
	children, err := f.childEntries(ctx, res, write)
	...
	out = append(out, children...)
	root.collToken = tokenFromChildren(children)   // <- local copy, out[0] already taken
}
```
**EXECUTED PROOF (parent, webdav package):**
```
Depth 1 root getetag = ""
Depth 0 root getetag = "dir-8419b82681603009"
```
The non-root path at `propfind.go:255-261` assigns through the slice element and
is correct (parent probe PASS: `dir-106dcda13ae2cd67`).

Client-observable symptom: a client that PROPFINDs the root at Depth 1 (the
normal listing depth) gets an empty ETag and must fall back to a full walk,
while the same collection at Depth 0 gives it a token. M2 and M3 COMPOUND: on a
single-bucket (mode B) server the Depth-1 root token is empty, so a syncing
client has no token at the top level at all.

Fix: `out[0].collToken = tokenFromChildren(children)`.

### M4. The oversized-copy pin now proves less than its name claims, and writes
### a package global mid-suite to do it.

`18b6a85` shrank `batchCopyMaxBytes` from 5 GiB to 1 MiB for the test's duration
(`internal/frontend/s3/batch_wrapper_actions_test.go:230-232`):
```go
prevCeiling := batchCopyMaxBytes
batchCopyMaxBytes = testCeiling
t.Cleanup(func() { batchCopyMaxBytes = prevCeiling })
```
The bound assertion (`batch_wrapper_actions_test.go:267`) is `read >
batchCopyMaxBytes+1`, which is now evaluated against the 1 MiB test ceiling. The
CI-57s-to-0.03s result is real and the reasoning ("the bound assertion does not
depend on the ceiling's magnitude") is correct. Two honest caveats:
- The name still says "OverSized"; the test no longer exercises a size any real
  client could produce. That is a deliberate trade, and the commit message says so.
- `batchCopyMaxBytes` is a plain package `var` written by a test. The s3 package
  HAS 10 `t.Parallel()` calls (`rangespan_test.go`). Parent verified the suite is
  green today: `go test ./internal/frontend/s3 -count=1 -race` = `ok 6.224s`. So
  no race fires today, but the pattern (a test mutating a package global another
  test reads) is the documented fragile-global shape.

---

## LOW

### L1. The main package's test suite is order-dependent at the new HEAD.

**EXECUTED (parent):**
```
go test . -count=1                  -> ok  22.4s   (0 failures)
go test . -count=1 -shuffle=1       -> ok  20.9s   (0 failures)
go test . -count=1 -shuffle=12345   -> FAIL, 138 failing tests
go test . -count=1 -shuffle=99999   -> FAIL
go test . -count=1 -shuffle=on      -> FAIL
```
Sample failures at seed 12345:
```
--- FAIL: TestCreateBucketHandler_Success
    main_handler_test.go:104: bucket directory was not created
--- FAIL: TestCompleteMultipartUploadHandler_EmptyPartsListInvalidPart
    multipart_handlers_test.go:1177: status = 404, want 400
--- FAIL: TestPresignedGET_HappyPath
```
The green baseline in `make test` does not see this because the suite runs in
declaration order. PROVENANCE (see X1 below): this is NEW in `269c193`, proven by
running the same seeds against `f54098c` in a throwaway worktree, where they all
pass. The commit that fixed this class one commit earlier (`906e8af`) now has a
commit message claiming all four seeds green, which is false at HEAD.

### L2. Untracked scratch state, plus one stale credential in the new
### `run-one.sh`.

`git status` at HEAD: `?? h3probe` (12 MB binary), `?? scripts/e2e/run-one.sh`,
`?? .hermes/audit-notes/` (now committed with this report's sibling). Also
`.claude/mind.mv2`, `scripts/e2e/__pycache__/`, `mini-s3`, `mini-s3-server` are
gitignored (verified with `git check-ignore -v`).
`scripts/e2e/run-one.sh:42` still exports the OLD renamed credential:
```sh
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
```
while every other case and the server default moved to `zetaadmin` in `a7f5efb`.
An operator running that helper against a server using the new default gets a
401 on every case and will likely read it as a broken server.

---

## Prior findings: re-verified verdicts

Re-located by grep at HEAD `269c193`, not by trusting the reported line numbers.

| Prior | Verdict | Note |
|---|---|---|
| C1 traversal escape | **FIXED (fully pinned)** | 5 gate sites: `dispatch.go:374` (covers the whole object surface), `object_handlers.go:1766`, `object_handlers.go:886`, `versions_listing.go:63`, `multipart_handlers.go:777`, plus the pre-existing `bucket_handlers.go:233/216/262`, `versioning_handlers.go:83/137`, `batch_bridge.go:74` and `resolveEventsContext`. Parent ran the revert matrix on the 5 NEW sites: every one fails `TestBucketNameGate_TraversalNameNeverResolvesToTheParentDirectory` on production-only revert. A 20-name input matrix finds ZERO accepted-and-escapes (Z1). |
| H1 hot-patch dishonesty | **STILL OPEN** | `GET /config DataDir` reports the live root while the installed view carries the candidate. Parent probe re-run at HEAD. |
| H2 `zfs_binary` installed while reported restart-required | **STILL OPEN (UNVERIFIED live)** | `copyAppliedKeys` has a `case "zfs_binary"` (`config_store.go:255`) unreachable because `hotApplyKeys["zfs_binary"]` is false; `installZfsDatasetProvisionerFor` (`s3_wiring.go:320`) still reads `cfg.ZfsBinary`. The live provisioner path needs a ZFS host to execute, so I did not prove it by run - UNVERIFIED. |
| H3 region data race | **STILL OPEN** | Now H1 above, proven by `-race`. |
| M2 batch move half-success | **NOT RE-CHECKED** | Auditor scope that owned it died. |
| M4 `?delete` hard-deletes | **STILL OPEN** | Now M1 above, proven by execution. |
| M5 test surface in the binary | **REFUTED as stated, but see note** | `2e0d736` moved `ValidateObjectKey` into a production file (`keyvalidate.go`) and added a build constraint path for the rest; no `testing` import remains in the shipped s3 `.GoFiles` at HEAD (parent did not re-verify with `go list`, so this is read-level). |
| M8 harness 0/0 PASS | **NOT RE-CHECKED** | Owned by the dead bucketmanager/harness auditor. |
| `707c4a1` gate claim | **HOLDS** | Enumeration above is the proof. |
| `2e0d736` `Unwrap` claim | **PARTIAL** | s3's `statusRecorder` has it (`audit_log.go:150`), `altsvc.go:80` has it, but the ADMIN twin at `internal/frontend/admin/routes.go:108` does NOT - see L3. |
| `18b6a85` ceiling claim | **HOLDS with the caveat in M4** | |

### L3. (added) The admin `statusRecorder` still blocks `http.ResponseController`.

`internal/frontend/admin/routes.go:107-110`:
```go
type statusRecorder struct {
	http.ResponseWriter
	status int
}
```
No `Unwrap()` method anywhere in that file (`grep -rn 'func (.*) Unwrap()' ` returns
only `altsvc.go:80` and `audit_log.go:150`). It is live at `routes.go:135`. Commit
`2e0d736` fixed the s3 twin and the s3 commit message explicitly names the
defect, so this is a fix that stopped one layer short.

---

## AGENTS.md rule compliance for this wave

- **E2E coverage rule (VIOLATED).** `git diff --stat b150a85~1..HEAD -- scripts/e2e/cases/`
  shows cases 18, 19, 34, 35, 38, 39 touched, so this wave is BETTER than the prior
  one on the letter of the rule. But `53aaaaa` (the new collection-token feature)
  added e2e assertions, while `269c193`, `f54098c`, `906e8af` and `a7f5efb` changed
  user-visible config/auth behavior with no new case.
- **ZFS validation rule.** `M1` changes delete semantics on a versioning-Enabled
  bucket, which is a data-plane change: it needs
  `scripts/zfs-validate/run-zfs-validation.sh` against `zfs-meta` before it counts
  as done. This audit did NOT run it (no ZFS contact), so M1's fix is gated on it.
- **Coverage floors.** No commit in this wave moved code between packages, so
  `COVER_MIN 47` does not need re-measuring. The CI split (`86876b2`) verified by
  reading the diff: the two new commands (`-race` alone, then `-coverprofile`
  alone) union to the old `-race -coverprofile`, and the `cover.out` floor check
  still runs on both paths. No gate was dropped.

---

## GATE-HONESTY PASS (parent-direct, replaces the dead auditor)

The gate-honesty scope's auditor died on the provider outage. The parent ran it.

### Gate summary table

| Gate | Command | Can it report green falsely? | Why |
|---|---|---|---|
| `make test` | `go test -count=1 -coverprofile=coverage.out ./...` | **YES** | Declaration order only. `-shuffle=12345` = 138 failures (L1). |
| `make test-race` | `go test -race -count=1 ./...` | **YES (narrowly)** | Green today (s3 6.2s) but H1's region race is not caught: no test drives `SetRegion` concurrently with a request reader. The race exists and the gate is silent on it. |
| `make lint` | `golangci-lint run ./...` | No | Parent ran it: `0 issues`, exit 0. |
| `make vet` | `go vet ./...` | No | Parent ran it: exit 0. |
| `make fmt-check` | `gofmt -l $(git ls-files '*.go')` | No | Fails on any listed file. |
| `make test-cover-enforce` | awk compare of `total:` vs `COVER_MIN=47` | **Weakly** | Aggregate measured **80.5%** vs floor 47. The floor is 33 points below reality, so it cannot detect a regression of any size this wave could cause. |
| CI per-package floors | `go tool cover -func` awk vs per-package `FLOOR=` | No, but see drift | Every measured value is at or above its floor. Several are far above: `internal/bucketmanager` floor 80 / measured 88.9, `internal/frontend/owncloud` 87 / 93.9, `internal/frontend/webdav` 79 / 84.7, `internal/frontend/ftp` 56 / 67.3. AGENTS.md says set the floor to the measured value; these were never ratcheted. |
| `make vuln` | `govulncheck ./...` | **YES** | `Makefile`: `which govulncheck || { echo "not installed, skipping."; exit 0; }`. A missing scanner is a green gate. (Tool IS present here: `No vulnerabilities found`, exit 0.) |
| `make secrets` | `gitleaks detect` | **YES** | Same `|| exit 0` shape. (Tool present: exit 0.) |
| `make e2e` | `bash scripts/e2e/run-e2e.sh` | No | `lib.sh:175` `[ "$E2E_FAIL" -eq 0 ]` is the exit condition; a case that crashes or bails before asserting is counted as a FAIL (`run-e2e.sh:267-280`). Verified by reading, not by running (see ledger). |
| CI job (86876b2 split) | `-race` then `-coverprofile` for s3; else both | No | Diff read: the two commands union to the old `-race -coverprofile`, and the `cover.out` floor awk still runs on both branches. No flag dropped, no `continue-on-error`. |

### G1 (LOW). `make vuln` and `make secrets` exit 0 when the scanner is absent.

`Makefile`:
```make
vuln:
	@which govulncheck > /dev/null 2>&1 || { echo "govulncheck not installed, skipping. ..."; exit 0; }
secrets:
	@which gitleaks > /dev/null 2>&1 || { echo "gitleaks not installed, skipping. ..."; exit 0; }
```
Both are part of `make check`, the full local gate. A developer without gitleaks
gets a green `check` with zero secret scanning. `lint`, `fmt-check` and `e2e` all
fail loudly when their tool or input is missing; these two are the outliers.
Fix: `exit 1`, or gate them behind an explicit `SKIP_SCANNERS=1`.

### G2 (INFO). Every coverage floor is below its measured value; the aggregate floor is 33 points below.

Parent-measured at HEAD `269c193`:

| Package | CI floor | Measured |
|---|---|---|
| internal/adminserver | 84 | 88.4 |
| internal/backend | 91 | 93.9 |
| internal/backend/conformance | 77 | 79.0 |
| internal/backend/fsbackend | 83 | 84.2 |
| internal/bucketmanager | 80 | 88.9 |
| internal/frontend | 91 | 93.3 |
| internal/frontend/ftp | 56 | 67.3 |
| internal/frontend/s3 | 83 | 83.2 |
| internal/frontend/sftp | 66 | 69.8 |
| internal/frontend/webdav | 79 | 84.7 |
| internal/frontend/owncloud | 87 | 93.9 |
| internal/fslock | 98 | 100.0 |
| internal/metadata | 87 | 92.9 |
| internal/objectmodel | 90 | 92.8 |

Aggregate: **80.5%** measured vs `COVER_MIN := 47`.
`internal/frontend/s3` is 0.2 points above its floor, so the next test that
removes a covered line turns CI red. Not a bug; it is the ratchet the AGENTS.md
rule asks for, applied to the wrong numbers. No package moved between packages
this wave, so nothing in this wave CAUSED the drift.

### G3 (INFO). Doc claims verified - all eight check out.

Each claim was located in code, not taken on trust:

| Claim | Implementing site |
|---|---|
| strict SigV4 region compare | `internal/frontend/s3/region.go:69` `checkRegionMatch` |
| collection change tokens / getetag on collections | `internal/frontend/webdav/colltoken.go`, emitted at `props.go:108`, computed at `propfind.go:260` and `:303` (the latter is M3) |
| delete markers | `internal/frontend/s3/versioning_handlers.go:388` `deleteObjectVersionedMarker` |
| JSON `?batch` operations | `internal/frontend/s3/batch_endpoint.go:79` `handleBucketBatch` |
| `/auth/reload` rotates the QUIC client CA too | `admin_wiring.go:553` `reloadRegisteredClientCAs`, fan over `clientCAReloaders`; registrants at `frontends.go:166` (admin) and `frontends.go:207` (h3). Registration keys on frontend `Name()` and REPLACES a matching entry (`admin_wiring.go:499-505`), so both listeners rotate and re-construction is idempotent. |
| ZFS bucket datasets | `internal/frontend/s3/zfsdatasets.go:183` `InstallZfsDatasetProvisioner` |
| HTTP/3 (QUIC) frontend | `internal/frontend/h3/frontend.go`, `internal/frontend/h3/serve.go` |
| ownCloud OCS | `internal/frontend/owncloud/{capabilities,owncloud,user}.go` |

### G4 (INFO). The e2e credential pair is genuinely enforced.

`run-e2e.sh:257` exports `AWS_ACCESS_KEY_ID=zetaadmin` / `AWS_SECRET_ACCESS_KEY=zetaadmin`
into every case; `config.go:24` `defaultAccessKey = "zetaadmin"` and
`config.go:642-643` build the env-pair identity from it. The generated config
(`run-e2e.sh:218-225`) sets no `auth` key, so the server runs in REQUIRED-auth
mode, not the `none` dev wildcard. Case `18-auth-identities.sh` additionally
asserts that an unregistered key is rejected with `InvalidAccessKeyId` (18d), so
a case that authenticated without the credential would fail. `scripts/e2e/run-one.sh:42`
is the one exception and still exports the old `minioadmin` pair (L2).

### G5 (INFO). 40 e2e cases exist; `git diff` shows 6 were touched this wave.

`ls scripts/e2e/cases/*.sh | wc -l` = 40. Touched: 18, 19, 34, 35, 38, 39.
No case carries a default skip marker (parent grepped for skip/continue/UNIMPLEMENTED
patterns and found none that gates a case out).

### G6 (LOW). The committed audit artifacts cite line numbers that no longer match the tree.

`.hermes/audits/2026-10-05-fix-wave-regression.md:41-43` cites
`object_handlers.go:1758` for DeleteObjects and `:883` for listObjectsV2. At
HEAD the same handlers are at `object_handlers.go:1760` and `:881`. The drift is
small (2-3 lines) and the FINDINGS are still correct, but a future reader who
jumps to the cited line lands on the wrong statement. This is the documented
line-ref-drift failure mode; the fix is to cite the symbol name first and the
line second.

---

## CROSS-CUTTING SWEEP (parent-direct, replaces the dead verifier)

The cross-cutting verifier's auditor died on the provider outage. The parent ran
steps 1-4 itself. Steps 1 and 2 are folded into the tables above; steps 3 and 4
are new.

### Step 2 results: commit-message claim audit

Every `Pins:` test name claimed by the wave's commits exists in the tree
(3 claimed, 3 found, 0 missing). Count claims:

| Commit | Claim | Verdict |
|---|---|---|
| `269c193` | `full e2e 881/881` | UNVERIFIED (no e2e run by parent) |
| `f54098c` | `go test ./... 21/21 ok` | **HOLDS** - parent: 21 packages ok, 0 FAIL |
| `a7f5efb` | `21/21 ok`, `make e2e 881/0 (40 cases)` | 21/21 HOLDS; 40 cases HOLDS (`ls scripts/e2e/cases/*.sh \| wc -l` = 40); 881/0 UNVERIFIED |
| `53aaaaa` | `case 19 part 19g: 72 asserts (was 65)` | UNVERIFIED (case not run) |
| `549d238` | `0/0` | This is the floor-addition commit; the `0/0` string is its own prior-quote, not a new claim |
| `906e8af` | `go test -shuffle on 4 seeds (4/7/11/23) all ok` | **FALSE at HEAD** - parent re-ran the SAME four seeds: 4 PASS, **7 FAIL**, 11 PASS, 23 PASS |
| `857df74` | `21/21 ok`, `make e2e 865/0` | 21/21 HOLDS; 865/0 UNVERIFIED |

`906e8af`'s shuffle claim was TRUE when it was written. It stopped being true
because `269c193` (the NEXT config commit) converted ~24 test files to a helper
that publishes a new config generation without a restore. See L1, now upgraded
with provenance.

### Step 3 results: dead-twin / live-caller grep

| Fixed function | Non-test callers | Verdict |
|---|---|---|
| `validBucket` | `dispatch.go:374`, `object_handlers.go:1766/:886`, `versions_listing.go:63`, `multipart_handlers.go:777`, `bucket_handlers.go:233/216/262`, `versioning_handlers.go:83/137`, `batch_bridge.go:74`, `resolveEventsContext` | LIVE - 12 call sites, no dead twin |
| `Env.validateName` (bucketmanager) | `create` (`bucketmanager.go:167`), `delete` (`delete.go:36`), `exists` (`bucketmanager.go:287`) | LIVE - all three entry points validate |
| `DeleteObjectVersionedMarkerForBucket` | `webdav/versioning.go:121` via `deleteMarkerOrPlain`, called from `delete.go:52` and `versioning.go:152` | LIVE |
| `statusRecorder.Unwrap` (s3) | `audit_log.go:150` method, wrapper constructed at `dispatch.go:112` | LIVE on the s3 surface; the ADMIN twin has no Unwrap (L3) |
| `ValidateObjectKey` (moved out of the test surface) | `webdav/batch.go:66`, `batch_bridge.go`, `keyvalidate.go` | LIVE |
| `s3BatchExecutor.resolveBucketPath` | `batch_endpoint.go:201/217/220/230` | LIVE - the previously-dead `bucketPath` parameter is now threaded |

No dead fixes found in this wave. That is a change from the prior wave's record
and is worth stating: the fixes landed on live paths.

### Step 4 results: cross-package class sweeps

**4.1 Setter/reader mutex asymmetry on new globals.** The wave added five globals:
`batchCopyMaxBytes`, `c1TraversalNames`, `configMu`, `httpAuthSentinels`,
`serverConfigAtom`. Four are read-only after init (`c1TraversalNames`,
`httpAuthSentinels`) or test-only (`batchCopyMaxBytes`, see M4). `configMu` is a
writer-only latch with the atomic pointer as the reader sync. **The one unguarded
reader is the SigV4 region global** - H1 - which this wave did not add but did
make hot.

**4.2 Resource leaks.** Every `be.Get(...)` call site in non-test code closes its
reader: `ftp/driver.go:535` closes via `defer rc.Close()` at `:546`;
`sftp/driver.go:79` closes at `:84`; `batch_endpoint.go:301` reads through
`io.LimitReader` inside a scope that closes. No discard-into-underscore on a
reader remains. The `_, _ = w.Write(...)` sites added by the wave are
`ResponseWriter` writes whose errors are irrelevant after a status is written.

**4.3 Error dropping.** The wave's `//nolint` additions are all `//nolint:errcheck
// server-side close.` on `defer r.Body.Close()` (intentional) and `//nolint:gosec
// G703` with a name-validation justification. Two of those G703 justifications
are the prior report's M7 class and remain UNVERIFIED here (the repo-wide-exclusion
claim was already found false in the prior audit).

**4.4 Context propagation.** The wave FIXED the batch surfaces:
`batch_endpoint.go:163` `itemCtx` prefers the caller's context so a disconnect
cancels remaining items. The one remaining per-request `context.Background()` is
`object_handlers.go:746` in `deleteObjectCore`, which is inside M1 and is the
same defect the prior report named. The rest are legitimate detached-lifetime
uses (signal contexts, drain deadlines) or the ftp/sftp drivers, which have no
per-request context in their library signature.

**4.5 Struct-vs-copy-list drift.** `ServerConfig` has 21 fields.
`deepCopyServerConfig` (`config_store.go:834`) explicitly clones all 8
reference-shaped fields (`Buckets`, `BucketBackends`, `BucketAuditReads`,
`BucketReflinkRetention`, `Backends` + its nested `Options`, `Frontends` + nested
`Options`, `Identities` + nested `Grants`/`SSHPublicKeys`, `AuditLog`) and clears
`bucketsErr`. The 13 remaining fields are scalars or `AuthConfig{Mode string}`,
which is scalar-only. **No drift.** This is the check that has bitten prior waves
and it passes.

**4.6 e2e coverage rule.** `git diff --stat b150a85~1..HEAD -- scripts/e2e/cases/`
shows 6 cases touched (18, 19, 34, 35, 38, 39). Commits changing user-facing
surface with NO e2e change: `269c193`, `f54098c`, `906e8af` (config/auth
behavior), `a7f5efb` (the credential rename itself, though 18/26/27 were updated
in the same commit), `f54098c`. `857df74` and `53aaaaa` both extended case 19.
Verdict: partially compliant, same as the prior wave.

**4.7 Coverage floors.** No commit moved code between packages, so no floor
required re-measuring by the AGENTS.md rule. `86876b2`'s split verified in the gate
table above: no flag dropped. Floors are nonetheless all below measured values
(G2).

### X1 (MEDIUM, upgraded from L1 with proven provenance). The order-dependence is
### NEW in `269c193`, and the commit that fixed it in the prior wave now lies about it.

Parent evidence, same seeds, two trees:

| Tree | `-shuffle=4` | `-shuffle=7` | `-shuffle=11` | `-shuffle=23` | `-shuffle=12345` | `-shuffle=99999` |
|---|---|---|---|---|---|---|
| `f54098c` (pre-atomic) | - | **PASS** | - | - | **PASS** | - |
| `a1b971a` (HEAD) | PASS | **FAIL** | PASS | PASS | **FAIL** (138 tests) | **FAIL** |

Provenance: `269c193` converted 24 test files to `setServerConfigField`
(`config_atomic_test.go:11`), which publishes a new config generation with no
cleanup. Of the 24 files that touch the global, 21 register no restore at all
(counted by grepping each file for a save/restore marker).
`config_identities_test.go:195` is the sharpest case: it mutates THROUGH the
loaded pointer -
```go
setServerConfigField(func(c *ServerConfig) { c.Identities = tc.identities })
serverConfig().Auth.Mode = tc.mode
```
- writing the second field directly into the currently-published generation,
which no later save/restore can undo because the save happened before.

Impact: `make test` is green because it runs in declaration order. A developer
or CI job that ever adds `-shuffle` gets 138 failures. `906e8af`'s commit message
claims all four seeds green; that was true then and is false now, and nobody
re-ran it.

Fix shape: make `setServerConfigField` register its own `t.Cleanup` restore from
the generation it snapshotted (it has no `*testing.T` today - change the
signature), and add `-shuffle=on` to a local gate target so the class cannot
return silently.

---

## HARNESS / BUCKETMANAGER / RENAME SWEEP (parent-direct, last dead scope)

The final auditor died on the provider outage. Parent ran it. This section
contains the most consequential finding in the report.

### Y1 (HIGH). The check-count floors that `549d238` claims to have added DO NOT
### EXIST. The helper is written, documented, and never called.

`549d238` commit message:
> Every check block now carries a count floor DERIVED from its own check
> definitions via an AST walk of the file (a truncated or skipped block is a
> FATAL naming the block and expected-vs-actual), so a run cannot pass on checks
> that never executed.

Parent evidence, `scripts/zfs-validate/run-zfs-validation.sh` (2794 lines):

```
$ grep -n 'require_floor' scripts/zfs-validate/run-zfs-validation.sh
292:# nothing (see the CHECK-FLOOR row require_floor appends). The floor is
353:def require_floor(block, results, branch_var=None, branch_val=None):
```

Two occurrences: one comment, one `def`. **Zero call sites.**

```
$ grep -n 'source.*checks\|import checks\|from checks' scripts/zfs-validate/run-zfs-validation.sh
NOT SOURCED ANYWHERE
```

The helper is materialized to `$WORK/checks-floor.py` at line 305 and never
imported by any of the five check blocks. The `CHECK-FLOOR` table the comment at
line 292 refers to does not exist either (`grep '# CHECK-FLOOR'` = 0 hits).

And the block the audit originally caught still has the original shape. Line
2758-2762, the section-15 exit path:
```python
failed = [(n, d) for n, ok, d in results if not ok]
for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"FIX_TOTAL: {len(results) - len(failed)}/{len(results)}")
sys.exit(1 if failed else 0)
```
`results == []` gives `failed == []` and `sys.exit(0)`. The identical pattern
exists at the four other section exits: `:1172` (`TOTAL:`), `:1359`
(`ZBD_TOTAL:`), `:1514` (`MGMT_TOTAL:`), `:2123` (`H3_TOTAL:`).

So `b150a85`'s headline claim "52/52" remains UNFALSIFIABLE, and
`549d238`'s fix for exactly that finding is dead code. `AGENTS.md` names this
harness as THE required gate for any data-plane or metadata change, which means
the repository's primary validation gate can report green having run nothing.

Three of `549d238`'s four other claims DO hold:
- the nested-collection emptiness check fails closed (`:2744` records a
  `check(..., False, ...)` on an undecodable body before the emptiness assert at
  `:2748`);
- the seven section-15 setup PUTs are status-checked (`:2483, :2485, :2487,
  :2711, :2713, :2730, :2732` each assert `status == 200`);
- ssh is invoked with `-o BatchMode=yes` throughout (21 sites).

One claim is FALSE, not merely unproven: `549d238` says "KEEP_SERVER at the final
cleanup no longer short-circuits on `$RC -ne 0`". The condition at line 2775 is
unchanged:
```bash
if [[ $RC -ne 0 || $KEEP_SERVER -eq 0 ]]; then
```
`$RC -ne 0` still short-circuits, so a FAILING run with `--keep-server` still
destroys the dataset the operator asked to inspect, immediately before `:2790`
prints "Server log follows."

Fix shape: call `require_floor` at the end of each block with the block's own
floor row, and re-order `:2775` to `if [[ $KEEP_SERVER -eq 0 ]]`.

### Y2 (INFO). bucketmanager is clean. All three entry points validate; the nil-Custom guard is pinned and passes.

- `Create` -> `bucketmanager.go:167` `e.validateName(name)` before any path work.
- `Delete` -> `delete.go:36` `e.validateName(name)` first statement after the
  not-installed guard.
- `Exists` -> `bucketmanager.go:287` `e.validateName(name)` before
  `e.BucketPath(name)` at `:290`.
- `checkContainment` (`validate.go:66`) rejects `""`, `"."`, `".."` and any name
  containing `/` or `\` - the backslash is checked deliberately so a config
  written on Windows cannot smuggle a separator onto a POSIX path.
- `validateNameRule` (`validate.go:87`) enforces 3-63 chars, lowercase alnum plus
  `-`/`.`, alphanumeric edges, no consecutive periods. Its comment at `:86`
  states the consequence: "The character set admits no separator and no leading
  dot, so every name it accepts is also containment-safe." Correct, and the
  dataset sibling inherits it (`deleteDatasetIfBacked` at `delete.go:120` builds
  `parent + "/" + name` from the same validated `name`, and ZFS dataset names
  forbid `/` anyway).
- `TestNilCustomIsNotInstalled` (`exists_guard_test.go:211`) covers Create,
  Delete and Exists. Parent ran it: 3/3 PASS, no panic.
- The prior M7 finding (a `#nosec` comment claiming G703 is "excluded
  repo-wide") is **FIXED**: `grep -rn 'excluded repo-wide' internal/ *.go` = 0
  hits. The remaining 16 `//nolint:gosec // G703` comments now carry local
  justifications naming the guarding call, and `.golangci.yml:29` enables gosec
  with only the listed exclusions - G703 is live, so the suppressions are real
  suppressions with real reasons.

### Y3 (LOW). The credential rename is complete except one file, and carries no migration note.

Remaining old-credential hits, all text files, excluding `.git` and the audit
report itself:
- `scripts/e2e/run-one.sh:42` - `export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin`.
  The new untracked helper. Every other case uses `zetaadmin`.

`MinIO` product references that legitimately remain:
- `scripts/e2e/cases/13-interop-mc.sh:19` and `:29` - the `mc` client download
  URL (`github.com/minio/mc/releases/...`). That is the upstream product being
  tested for interop, not a credential reference. Correct to keep.

Migration note: absent. `a7f5efb` is a `refactor!` (breaking) change to the
default credential with no CHANGELOG (the repo has none) and no note in
README.md's env-var table (`README.md:113-114` just states the new default).
An operator whose config.json or systemd unit sets
`ZETAOBJECT_ACCESS_KEY=minioadmin` keeps working unchanged (the env pair is
explicit and wins over the default), so the blast radius is narrow - but an
operator who RELIES ON the default and never set it silently gets new
credentials. The note belongs in README.md next to the env-var table.

### Y4 (INFO). The `-mod=mod` fix is complete and h3probe is correctly isolated.

Every `go run` in `scripts/` carries the flag:
- `scripts/e2e/cases/18-zmetad-events.sh:61` - `go run -mod=mod ./scripts/e2e/fixtures/zmetad-fixture`
- `scripts/e2e/cases/38-h3-webdav.sh:143` and `:160` - `go run -mod=mod ./scripts/e2e/h3probe`

`h3probe` is a separate `package main` in the module, so `go build ./...` builds
it as its own binary. Parent verified it is NOT linked into the server:
`go list -deps . | grep h3probe` = no match. `go vet ./scripts/e2e/h3probe/` =
exit 0. The untracked 12 MB `./h3probe` binary at the repo root is local build
output, not a tracked artifact (see L2).

---

## BUCKET-NAME GATE SWEEP (parent-direct, last dead scope)

The s3-gate auditor died on the provider outage. The parent had already
enumerated the gates during pre-reads; this section closes the four remaining
questions (input matrix, revert matrix, post-validation mutation, e2e case).

### Z1 (INFO). The C1 gate is COMPLETE and every one of its 5 sites is pinned. No name escapes.

`validateBucketName` (`internal/frontend/s3/bucket_handlers.go:281`) enforces:
3-63 characters; first and last byte in `[a-z0-9]`; every byte in
`[a-z0-9.-]`; no consecutive periods; not an IPv4 literal
(`:307`). `validBucket` (`bucket_handlers.go:40`) additionally exempts any name
present in the configured custom-bucket map.

**EXECUTED GATE MATRIX (parent, s3 package, `/tmp` overlay):** 20 candidate
names, one custom bucket configured as an exemption:

```
REJECTED  ".."  "."  "..."  "../escape"  "a/b"  "a\b"  "/etc"
          "..%2f..%2fetc"  "ab"  ""  "photos "  " photos"  "photos\n"
          ".photos"  "photos."  "1.2.3.4"  "PHOTOS"  "caf\u00e9x"
ACCEPTED  "a.b"   -> /DATA/a.b        (safe)
ACCEPTED  "a-b-c" -> /DATA/a-b-c      (safe)
```

**Zero ACCEPTED-AND-ESCAPES.** Notable by construction: the character class
admits no `/`, no `\`, no space, no NUL and no non-ASCII byte, so no accepted
name can contain a path separator, and `filepath.Join` cannot clean a separator
that is not there. `..` and `.` are additionally rejected by the length/edge
rules independently of the separator rule (`..` fails the edge rule; `.` fails
the 3-character minimum). Case-folding is not a hole: uppercase names are
rejected outright, so a bucket `Foo` and `foo` cannot collide across a
case-insensitive APFS mount and a case-sensitive ext4 - the S3 surface simply
has no uppercase bucket.

**REVERT MATRIX (parent, `/tmp` overlays, production file only, tests kept):**

| Gate reverted | Exit | Failing test |
|---|---|---|
| `objectLevelDispatch` (`dispatch.go:374`) | 1 | `TestBucketNameGate_TraversalNameNeverResolvesToTheParentDirectory` |
| `deleteObjectsHandler` (`object_handlers.go:1766`) | 1 | same |
| `listObjectsV2Handler` (`object_handlers.go:886`) | 1 | same |
| `listMultipartUploadsHandler` (`multipart_handlers.go:777`) | 1 | same |
| `listObjectVersionsHandler` (`versions_listing.go:63`) | 1 | same |

All five sites are pinned, and by ONE test that asserts the FILESYSTEM after
each request rather than a status code. That is the stronger assertion shape
and it is why the pin catches a gate removal on any of the five.

**Gate ordering.** The gates run AFTER authentication and AFTER the grant
decision (`dispatch.go:96` authorize, then `:104` routeAuthorized, then
`:374`). That ordering is correct for this finding: auth still applies, so a
traversal name cannot reach a handler unauthenticated, and a traversal name with
a valid identity gets 404 `NoSuchBucket` - confirmed by case 39's assertions
(13 refusal assertions, all expecting 404, plus a positive control per surface).

**Header-derived bucket names are gated too.** `x-amz-copy-source` is
`url.PathUnescape`d at `object_handlers.go:1509` into `srcBucket`, which is then
checked by `validBucket(srcBucket)` at `:1530` before any `getBucketPath(srcBucket)`
at `:1622` / `:1693`. The decode-then-validate order is the correct one; a
validate-then-decode order would have been a real bypass. It is not present.

**No post-validation mutation.** `grep -rn 'bucketName = |strings.ToLower(bucketName)'`
over the s3 package returns exactly one assignment, `dispatch.go:49`
`bucketName = pathParts[0]`, which is where the name ORIGINATES (before every
gate). `r.URL.Path` is already percent-decoded by `net/http` before
`serveHTTP` sees it, so the gate and `getBucketPath` read the same string. No
lowercasing, trimming, or second decode happens after a gate.

**The management API is covered by a different, equivalent validator.**
`internal/bucketmanager` funnels `Create`/`Delete`/`Exists` through
`Env.validateName`, which enforces containment (rejects `""`, `"."`, `".."`,
and any `/` or `\`) plus the same S3 naming rule (Y2). So the console's
`/api/buckets/{name}` route and the s3 wire enforce one rule set in two places,
not two rule sets.

### Z2 (INFO). e2e case 39 cannot pass by refusing everything.

`scripts/e2e/cases/39-bucket-name-gate.sh` is honest in the way that matters:
it runs a POSITIVE CONTROL on every surface before the traversal refusals (PUT
200, GET 200, ?list-type=2 200, ?versions 200, ?uploads 200, ?delete 200, then
a 404 proving the ?delete really deleted; the canary is re-seeded and ?batch
gets its own 200). A server that refused everything would fail the first
control.

It also pins the forms that matter, and says why: every traversal path is
percent-encoded, with an explicit comment that a literal `/..` would be cleaned
by `http.ServeMux` into a 307 before any handler ran, and an instruction not to
weaken the assertions to literal dots. Uppercase `%2E%2E` is pinned separately
(`case 39` line ~86), as is the single-dot form `/%2e`.

One honest gap: the case asserts STATUS and error CLASS (`NoSuchBucket`) on the
wire, but not the filesystem. The unit pin (`bucket_name_gate_test.go`) is the
one that asserts the filesystem, and case 39's header comment says so. Together
they cover it; neither alone would.

**No `|| true` and no unfalsifiable grep in case 39.** Every assertion is an
`assert_eq` against an exact status code or an `assert_contains` against the
`<Code>` element extracted from a body the server just produced.

---

## FIX WAVE - DISPOSITION (2026-10-06, all findings closed)

Every finding in this report has a landed fix and a pin that was proven
NON-VACUOUS by the revert matrix (revert the production file alone via a `/tmp`
`go test -overlay`, keep the new test, confirm it fails).

| ID | Fix | Pin | Revert proof |
|---|---|---|---|
| Y1 | 5 `require_floor` call sites + `exec(open("checks-floor.py"))` in every block; `--keep-server` no longer short-circuits on `$RC` | floor fires on 0 recorded / 1 of 3 recorded; 0/0 now exits 1 | n/a (the helper had NO call sites; this is the fix) |
| Y1-bug | the floor helper ALSO could not run: `_self_source()` and `_BLOCK` used a bare `os` against `import os as _os`, and `_unconditional` excluded `try:` bodies so `checks-mgmt.py`'s `ds` arm floored at 0 | all 5 floors measured non-zero and satisfiable (82/33/10+7/26/55) | n/a |
| H1 | `region` is `atomic.Pointer[string]`; `regionOf`/`regionExplicit` load it | `TestRegionConcurrentSetAndRead` (-race), `TestRegionExplicitSemanticsAfterAtomicSwap` | SAME body on `269c193`'s plain var: `WARNING: DATA RACE` at region.go:17/30 |
| H1/H2 | `applyHotSeams` receives `live + applied keys`, never the raw candidate | `TestHotPatchKeepsRestartRequiredKeyOutOfTheLiveView` | reverts to `applyHotSeams(&candidate)`: FAILS with the view/snapshot divergence |
| H3 | the fs-root resolver reads the installed view per call (`BucketPathFromInstalledView`) | `TestHotPatchResolverFollowsTheInstalledView`, `TestCustomBucketGateAndResolverAgree` | reverts to `bucketPathFor(cfg, ...)`: both FAIL |
| M1 | `deleteBatchExecutor.Delete` consults `deleteObjectVersionedMarker` first; `deleteObjectCore` takes a context | `TestDeleteObjectsRecordsDeleteMarkerOnVersionedBucket`, `TestDeleteObjectsUnversionedBucketStillHardDeletes` | reverts to the direct hard delete: FAILS |
| M2 | `collectionToken` takes the marker consult and filters tombstoned children | `TestCollectionTokenMovesOnDeleteMarker`, `TestCollectionTokenDepth0AndDepth1Agree` | drops the filter: both FAIL |
| M3 | `rootEntries` assigns `out[0].collToken`, not the local `root` | `TestRootCollectionTokenSurvivesDepth1` | assigns to `root`: FAILS |
| X1 | `setServerConfigFieldT` registers a `t.Cleanup` restore; 9 call sites converted | `TestConfigHelperLeaksNothingAfterCleanup`; `make test-shuffle` over 6 seeds | seeds 7/12345/99999 went from 138 failures to 0 |
| L3 | admin `statusRecorder` gains `Unwrap()` | `TestStatusRecorder_ResponseControllerReachesUnderlyingWriter` (with a no-Unwrap control) | removes `Unwrap`: FAILS |
| G1 | `make vuln` / `make secrets` fail closed when the scanner is absent; `SKIP_SCANNERS=1` prints the skip | measured: no scanner -> exit 2 + FAIL message; `SKIP_SCANNERS=1` -> exit 0 + SKIPPED line | n/a |
| G2 | 14 CI coverage floors ratcheted to measured-minus-one; aggregate `COVER_MIN` 47 -> 80 | `make test-cover-enforce` = 80.4% vs floor 80 | n/a |
| M4 | documented as a deliberate trade (no code change) | the existing pin still fails on an unbound read | n/a |
| Y3 | documented; `run-one.sh` is untracked scratch, not committed | n/a | n/a |

### Two pins were VACUOUS on first write and had to be corrected

Worth recording, because both looked green on the broken code:

1. **The H1 pin passed on the reverted code.** Package main's tests register a
   config-sync hook (`testshim_test.go`'s `init`) that re-installs the s3 view
   from `serverConfig()` on EVERY consult. `serverConfig()` is not updated by a
   hot patch, so the hook overwrote whatever the patch installed. The pin now
   calls `s3.SetConfigSyncHook(nil)` and restores it in cleanup - production
   never installs one, so a production-semantics pin must clear it.
2. **The H3 layout pin passed on the reverted code** because it used a PLAIN
   bucket, whose path is `dataDir/name` under both implementations. It now
   uses a custom bucket, which is the only shape that distinguishes "reads the
   live view" from "reads the startup snapshot".

Also corrected mid-fix: the region race pin's first two assertions were wrong,
not the code. `regionExplicit()` is false in DEFAULT mode while `regionOf()`
returns `us-east-1` (its default), so `regionExplicit() == (got != defaultRegion)`
fails 29 times on correct code; and `checkRegionMatch` re-reads the region
internally, so a reload landing mid-call legitimately rejects.

### Gate results after the fix wave

```
go build ./...                    ok
go vet ./...                      exit 0
make fmt-check                    gofmt clean
make lint                         0 issues
go test ./... -count=1 -p 4       21 packages ok, 0 FAIL
go test -race -count=1 ./...      21 packages ok, 0 FAIL   (was green-but-blind to H1)
make test-shuffle                 6/6 seeds ok              (was 3 seeds failing)
make test-cover-enforce           80.4% vs floor 80
make parity-test                  pass
make mod-tidy-check               tidy
make vuln / make secrets          exit 0 (scanners present)
bash -n scripts/zfs-validate/…    clean; all 5 floors wired and non-zero
bash -n scripts/e2e/*.sh          all 40 cases parse
```

## Disclosure ledger - what this audit did NOT verify

1. **No auditor output exists.** All 7 parallel auditors plus 1 re-dispatch died
   on a provider outage. There is no independent second opinion anywhere in this
   report. Every finding here is parent-direct, which means no finding has been
   adversarially reviewed by a second agent - a known limitation, not a claim of
   completeness.
2. **No `make e2e` run.** The 40 cases were READ and the harness exit logic was
   traced to `lib.sh:175` and `run-e2e.sh:267-280`, but no case was executed.
   Every claim about what an e2e case can and cannot detect remains read-level.
3. **No ZFS contact.** `zfs-meta` was never reached. H2's live provisioner leg and
   M1's fix gate are both unverified against real OpenZFS. Y1 was proven
   STATICALLY (the call-site grep is conclusive: zero call sites for the floor
   helper), so it does not need a live run; but the FIX for Y1 does.
4. **No revert-matrix sweep.** (The parent did run it by hand for two findings: the
   webdav root token and the s3 delete asymmetry both reproduce on a real
   handler path, which is the same property a revert proves.) The technique that proves a pin fails on revert of
   the production file alone was applied by hand only to the webdav root token and
   the s3 delete asymmetry. The remaining new pins in `907a8af`-style commits
   (colltoken_test.go, propfind_marker_test.go, copymove_conflict_test.go,
   autherr_parity_test.go, batch_wrapper_actions_test.go) are UNVERIFIED as pins.
5. **H2's severity depends on a design call I did not make.** If the operator
   contract is "a restart-required key never moves the live plane", then
   authorities (3) and (4) being startup-frozen is CORRECT and only the view (2)
   is wrong - which makes H2 a MEDIUM (a `GET /config` honesty bug) and H3 a
   HIGH (the gate/read divergence is real either way). I ranked H2 HIGH because a
   four-way disagreement has no honest reading; the owner may rank it lower.
6. **L1's 138 failures were not root-caused individually.** The common shape (a
   test observing another test's leaked config generation) is proven for the
   CLASS by the worktree comparison and by the `config_identities_test.go:195`
   in-place mutation, but I did not attribute each of the 138 individually.
7. **Package-scoped sweeps not done**: resource-leak sweep over the diff's new
   `Get`/`Close` sites, error-dropping sweep (`_ =`, `//nolint`), context
   propagation sweep beyond M1, and the struct-vs-copy-list drift check against
   `deepCopyServerConfig`.
8. **Interop clients not exercised.** No boto3/mc/rclone/owncloudcmd run.
9. **Gates run by the parent:** `make lint` (0 issues, exit 0), `go vet ./...`
   (exit 0), `go test ./internal/frontend/s3 -count=1 -race` (ok 6.224s),
   `make vuln` (No vulnerabilities found, exit 0), `make secrets` (exit 0),
   per-package coverage for all 14 floored packages, and `-shuffle` seeds 1 /
   12345 / 99999 / on over package main. NOT run: `make check` (the composed
   full gate), `make build`, `make fmt-check`, `make e2e`, `make parity-test`,
   `make conformance`, govulncheck against a fresh DB.
10. **The tree is byte-identical to how it was found**, except this report file.
    All probes ran via `go test -overlay` against `/tmp`.

## Next steps, ranked

1. Fix Y1 FIRST: the repo's declared data-plane gate can report green having run
   nothing, and `549d238`'s fix for it is dead code. That outranks every code
   finding in this report.
2. Fix H1: `atomic.Pointer[string]` for the region global; unblocks `-race` CI.
3. Fix H3 with H2: one bucket-path authority the gate and the resolver both read.
4. Fix M1: marker-first order in `deleteBatchExecutor.Delete`, thread the request
   context, then run the ZFS validation harness before calling it done (the
   harness needs Y1 fixed first, or the green run proves nothing).
5. Fix M2 + M3 together (both are the token feature's correctness; M3 alone leaves
   the Depth-1 root empty).
6. Fix X1: give `setServerConfigField` a `t.Cleanup` restore, then add `-shuffle`
   to the local gate so the class cannot return.
7. Add `Unwrap()` to the admin `statusRecorder` (L3); gitignore or delete the
   untracked scratch; fix `run-one.sh`'s credential (L2/Y3); add a migration
   note for the breaking credential rename.
