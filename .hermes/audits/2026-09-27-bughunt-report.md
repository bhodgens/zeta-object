# mini-s3 bughunt — 2026-09-27

Baseline: go build / vet / test green, 50.0% coverage (`go test ./... -count=1 -cover`).
Method: 6 parallel read-only auditors (SigV4, read path, write path/multipart, bucket
handlers, actions.go, config/lifecycle/tests) + parent verification of every HIGH at
file:line. No findings were refuted; one re-graded (traversal: latent, see V-2).
Fixes NOT applied — awaiting go-ahead.

## Verified HIGH findings

- V-1 main.go:2011 — timing-unsafe signature compare (`serverSignature != clientSignature`).
  No `hmac.Equal`/`subtle` anywhere. Fix: `hmac.Equal` + hex-format check on client sig.
- V-2 main.go:702,1536 + main.go:2079-2090 — `validateObjectKey` checks only length/null
  bytes; no `..` or `.metadata` segment rejection. Traversal currently blocked only
  incidentally by ServeMux path cleaning (version-dependent). Latent HIGH; defense-in-depth missing.
- V-3 main.go:1338,1474-1475,1699-1700 — `uploadID` from query string joined into
  filesystem paths unvalidated; `os.RemoveAll(partsDir)` reachable at attacker-chosen
  path. Fix: hex-only format check before use.
- V-4 main.go:1546 — multipart assembly writes directly into the live key
  (`O_TRUNC`, no temp+rename); error return at 1596-1601 does not remove the partial
  file. Crash/abort mid-assembly destroys the previous version of the object.
- V-5 main.go:127-174 — `decodeAWSChunked` discards `chunk-signature`; combined with
  main.go:1871-1875 accepting any `STREAMING-*` payload hash, streaming upload bodies
  have zero integrity verification. `streamingPayload` constant is dead code.
- V-6 actions.go:485-503 + 587 — `substituteVariables` interpolates attacker-controlled
  object keys into `sh -c` verbatim; command injection for any client who can PUT.
  Fix: arg-vector exec or shell-quote at substitution time.
- V-7 main.go:238 — hardcoded `:8443` on all interfaces (default creds zetaadmin),
  no Read/Write/Idle timeouts (slowloris), no graceful shutdown. Conflicts with
  scripts/test-s3-full.sh PORT env (script waits on $PORT, server ignores it).
- V-8 main.go:838-926,1002-1053 — no Range support at all (no 206/416/Content-Range);
  no conditional requests (If-Match/If-None-Match/If-Modified-Since). grep confirms
  zero `Range` references.
- V-9 main.go:891,1044 — Content-Length served from metadata, never checked against
  the on-disk file; divergence aborts responses or corrupts downloads. Compounded by
  V-4/V-11 non-atomic writes.
- V-10 scripts/test-s3-full.sh:751 — `print_results || true` forces exit 0; CI gets
  false passes. Also :251-267 PORT env is a lie (see V-7); presigned test (654-671)
  mis-counts and hard-fails because the server has no query-string auth at all.
- V-11 main.go:766,812 — non-atomic object data and metadata writes (plain
  os.WriteFile); concurrent GET can read a torn object or torn metadata JSON
  (permanent 500 for that key). No fsync anywhere.
- V-12 main.go:626-651 — deleteBucket empty-check/RemoveAll unsynchronized with
  concurrent PUT (object silently destroyed, delete returns 204); `.metadata`
  exclusion means DELETE succeeds with in-flight multipart uploads (real S3: 409).

## MED (verified or high-confidence)

- main.go:1808 — canonical query uses `url.QueryEscape` (space → `+`; SigV4 requires
  `%20`). Any query value with a space fails signature. main_test.go:358 ASSERTS the
  buggy `key=a+b` behavior as correct.
- main.go:1779-1786 — canonical URI re-encodes with Go's preferred escaping instead of
  the client's raw encoding; likely root cause of the "may fail with AWS CLI" note.
- main.go:1841 — canonical header values not space-collapsed (TrimSpace only).
- main.go:1977-1982 — SignedHeaders mismatch check logs but does not reject (no-op).
- main.go:1871-1875,131-159 — truncated/corrupt aws-chunked bodies silently accepted;
  no `x-amz-decoded-content-length` check; trailer checksums ignored.
- main.go:1149-1168,1226 — delimiter roll-ups don't count toward maxKeys; KeyCount can
  exceed MaxKeys; truncation position wrong vs S3.
- main.go:1172-1187 — max-keys=0 returns IsTruncated=true (real S3: false); can emit
  IsTruncated=true with empty NextContinuationToken (client pagination breaks).
- main.go:1201,390 — list keys not URL-encoded; `encoding-type=url` ignored (AWS CLI v2
  default) — keys with `%`/`+` mis-decode client-side.
- main.go:934-938 — DeleteObject on nonexistent bucket returns 204 (real S3: 404 NoSuchBucket).
- main.go:566-569 — createBucket treats any stat error as "exists" → 200; path-is-file → 200;
  BucketAlreadyExists/BucketAlreadyOwnedByYou never produced. Custom bucket create
  returns 200 even when the custom path is missing.
- main.go:689-691,660-666 — headBucket/GetBucketLocation: any non-NotExist stat error
  or file-not-dir → 200. `bucketExists` (line 90) is correct but dead code.
- main.go:533+ — XML responses lack `<?xml?>` prolog and S3 xmlns; `xml.Header` never used.
- main.go:1632 — completeMultipart takes Content-Type from the COMPLETE request (CLI
  sends none) instead of the INITIATE request → completed objects get empty Content-Type.
- main.go:1697-1730 — abortMultipart never validates uploadID belongs to objectName
  (uploadPart/complete do) — any valid ID abortable under wrong key.
- main.go:1270-1313 — no uploadID expiry/GC (AWS: 7-day auto-abort).
- main.go:812,1307,1435,1459,1649 — all metadata JSON writes non-atomic (torn JSON →
  permanent 500 for the object).
- actions.go:353-354 — inheritance `disable` mode identical to `override`; disables nothing.
- actions.go:552-570 — matchPathGlob simple branch matches as unanchored prefix
  (`logs/*` matches `logs/a/b/c`); `**` branch escapes only `.` (bad regex → silent false).
- actions.go:202-272 — stripJSON5Comments ignores single-quoted JSON5 strings;
  `//` inside them is stripped as a comment.
- actions.go:574-612 — timeout ineffective for descendant processes (no Setpgid/group
  kill); `timeout <= 0` = no deadline at all; unbounded output buffers.
- actions.go:116-118 — `timer.Stop()` result ignored → spurious inactivity fire at boundary.
- main.go:61-63 — invalid config JSON partially mutates global; config.json gets no
  JSON5 stripping while .bucket-actions does.
- main.go:100-107 — credentials read at package init; `MINIS3_ACCESS_KEY=""` silently
  reverts to zetaadmin default.
- Makefile:34 — self-signed cert has CN only, no SAN → cert verification fails even
  intentionally on modern clients. No tls.Config (TLS 1.0/1.1 accepted).
- scripts/test-s3-operations.sh — bare `set -e` (first AWS error aborts, no summary);
  missing set -u/pipefail; content-mismatch only logs, final line claims success.
- scripts/show-bucket-actions.sh:33 — string-unaware `sed` comment stripper (same bug
  class the Go tests guard against).
- Zero unit coverage: rootHandler, authenticateRequest, getCanonicalHeaders,
  getPayloadHash, loadConfig, and the whole action-execution runtime (runCommand,
  recordActivity, executeAction). 50% headline is carried by pure helpers.

## LOW

Signature-internals logged on mismatch (main.go:2012); region mismatch mis-coded 403
vs AWS 400 (main.go:1956-1959); scope-date mismatch error semantics (1948-1953); rigid
auth regex (177-179); RFC2616 Date fallback (1903); plaintext 405s not S3 XML
(main.go:276,303,336); bucket-level GET without list-type=2 serves V2 XML (297);
empty Content-Type → no default binary/octet-stream (888); multipart lock map never
evicted (109-116); whole-body buffering in RAM (726,1394); stray dotfile makes bucket
undeletable (627); metadata removed before bucket dir mid-delete failure state (637/646);
inaccessible custom buckets vanish from ListBuckets (472-478); symlink loops degrade
silently (500-504); undocumented concurrent same-key PUT tear (gap-closure.md #11
acknowledges); CLAUDE.md "Known Issues" stale (data-loss bug fixed, line numbers wrong);
dead test helpers (main_test.go:175-201, actions_test.go:253,317).

## Capability gaps vs real S3 (feature-completion list)

No Range/conditional GET; no presigned URLs (query auth absent server-side); no
CopyObject; no DeleteObjects batch (POST /?delete); no ListObjectsV1; no
encoding-type=url; no ListMultipartUploads/ListParts; no versioning; no checksums
(x-amz-checksum-*); no bucket subresources (policy/tagging/lifecycle/CORS/encryption/
notification); ACLs stubbed; single hardcoded credential; no config for listen
addr/port/cert paths; no non-TLS mode; no per-bucket/object chunk-signature chain;
no upload expiry; no streaming/size-limited writes; no fsync/durability; directory-
marker keys (trailing `/`) impossible; no request-id headers; no graceful shutdown/
health/metrics.
