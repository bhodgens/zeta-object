# Bughunt Wave: mini-s3 gateway arc (7f7ce35..HEAD)

Date: 2026-09-29. 5 read-only auditors, disjoint scopes. All HIGH findings
parent-source-verified before inclusion (4/4 confirmed). Race gate:
go test -race ./internal/... all 8 packages clean. Baseline at dispatch:
build/vet/full suite/e2e 155/155 green; test-cover-enforce RED at HEAD
(finding E1 - was already red before this wave's fixes).

## HIGH (4) - all verified

- A1: zfsSuperMagic 0x2f5f2f8b is wrong (actual ZFS_SUPER_MAGIC 0x2fc12fc1,
  zfs-metadata include/sys/fs/zfs.h:1427). DetectZFS always false on Linux -
  the zfs-events provider can never attach on its only target platform, and
  A5 swallowing hides why. internal/metadata/fsdetect_linux.go:8
- C1: zfs-events provider never registered in production (NewZFSEventsProvider
  zero non-test callers, no metadata import in wiring). Every ?events request
  503s forever. Also: nothing calls ProbeAndAttach in production.
- D1: custom-bucket path split-brain. backend_lookup.go roots custom buckets
  AT the custom path; fsbackend joins root+bucket (nested one level deeper);
  handler-side getBucketPath returns the bare custom path. Wire-visible layout
  change + CompleteMultipartUpload then NoSuchKey on custom buckets; ?versions
  and copy-source sidecar reads always empty on custom buckets. e2e has zero
  custom-bucket cases.
- E1/E2: coverage floor silently broken by the package split: aggregate 47.9%
  vs COVER_MIN=70; internal/frontend/s3 own-package coverage 24.3% but CI
  floor defaults to 70 for it. make test-cover-enforce red at HEAD; CI
  check.yml red. Makefile comment cites the stale 83.6% figure.

## MEDIUM (10)

- A2/C2: LastHistoryDetail package-global fallback: concurrent ?events on
  different buckets cross-attribute dataset/recordsLost (zfsEventsProvider
  lacks detailReporter).
- A3: ResolveDataset never compares mountpoint to bucket path (comment claims
  it); zfs get two-line output shape unverified on a real host.
- A4: AssertHeaderParity is exact-match; the 60s LM tolerance policy lives only
  in tests; ETag compare not normalized (inconsistent with ETagsMatch).
- A5: ProbeAndAttach swallows errors and ProbeResult.Reason with nothing logged.
- B1: list.go groupConsumedByCursor: a continuation token that is a plain key
  sharing the roll-up prefix silently consumes the whole delimiter group
  (probe-confirmed page loss).
- B3: Get opens the data file OUTSIDE the reader dir locks - TOCTOU vs
  cleanupEmptyDirs; ENOTDIR-class errors become 500.
- B4: Put locks flatPath but may write shadow path - cross-key prune/write
  windows under prefix aliasing.
- B5: filepath.Clean aliasing: Put(bucket, "a//b") and Put(bucket, "a/b")
  collide on the same object silently; "." yields late EISDIR/500.
- D2: 500 bodies now leak absolute filesystem paths + error strings (old:
  fixed strings).
- D3: ListBuckets on unreadable dataDir now 200-empty (was 500).
- D5 (inherited): ?versions max-keys=0 returns ALL versions.
- E3: e2e has ZERO coverage of backends/frontends config keys and ?events
  endpoints - 155/155 is legacy-only.
- E4: frontend conformance suite is vacuous (NotFoundHandler stub passes).
- E5: CLAUDE.md stale on >=5 claims (Go version, floors, file table, Range,
  shadow layout).

## LOW (24)

A6-A10, B2/B6-B12, C3-C10, D4/D6-D11, E6-E9 summarized in auditor transcripts
(live/deleg_515f71a5/task-*.log). Notables: C3 testshim.go compiled into prod
binary with runtime config-sync hooks; C4 eleven more unsynchronized seam
globals (same class as the fixed installedServerConfig race - latent);
B9 Delete trusts sidecar storagePath with no containment check; C5 dedicated
listener failures swallowed (_ = ListenAndServeTLS).

## Disclosure / boundaries

- Hardening + test-gap campaigns (earlier in the 2-day window) excluded:
  they had their own audit (docs/plans/hardening-2026-09, a518b37).
- Auditors left one scratch worktree: git worktree prune / rm -rf /tmp/ms3-anchor.
- Interleaved sibling commits (c334f75, ddb609e, 27524bc) audited where they
  touch scoped files (scope D); their own test-side content audited by E.
- E auditor claims commit 2bffc6c's message ("floors dry-run verified") was
  true at commit time, false after later commits - attribution noted.

## Clean coverage highlights

Lock ordering (no inversions), atomic write paths (temp cleanup complete),
sidecar byte-compat, multipart core (bounds/order/ETag), pagination cursor
exclusion semantics, xml.go md5-identical modulo package, auth fail-closed,
presigned parity, object_meta_batch worker pool race-free, config parse
atomicity, backend registry discipline.


---

## Regression review of fix waves (2026-09-29, commits 1bb0820/f8484b6/9cc0910/3b0a7cf)

Verdicts: 1bb0820 CLEAN; f8484b6, 9cc0910, 3b0a7cf sound with findings below.
All findings fixed in 9094993:

1. HIGH: ?versions NextKeyMarker not url-encoded under encoding-type=url
   (markers are keys; entry keys were encoded, markers were not). Fixed +
   pin test.
2. MED: B5 canonical-form rule rejected the S3-legal bare key "." (Clean
   folds "/." to "/"). Exempted with rationale; reject set otherwise
   unchanged; pin test.
3. MED: E6 half-done - per-bucket object form accepted unknown keys
   silently (top level was strict). DisallowUnknownFields applied; pin test.
4. LOW: D3 left a silent _ = add(f); now logs both add failures and
   backend-unavailable custom buckets.
5. LOW: 500 message inconsistency unified to legacy "Internal Server Error".
6. Verified sound by review (no action): B9 containment (filepath.Rel
   exact-prefix trap handled), B3/B4 lock ordering, A4/A6 parity+ETag,
   e2e cases 15-17 asserts are substantive.
7. Tracked debt (deliberate, not fixed): aliases.go (448 lines of
   export aliases) remains a production-linked non-test file pending a
   cleanup leaf; B10 (per-chunk ctx check in list gather) left from F2
   scope split - list.go single-file fix, deferred to next touch.
