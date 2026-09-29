# Mini-S3 Gap Closure Plan

This document identifies bugs and gaps in the zeta-object server and provides a plan for fixing them.

## Status: COMPLETED

All critical and high-priority issues have been fixed. The server is now fully functional with AWS CLI v2.

> **Note (2026-09):** the concurrency note in "Remaining Medium Priority Issues"
> below is superseded - per-key write serialization now exists via `lockObject`
> in `storage.go`, and multipart completion holds the object lock
> (`multipart_handlers.go`). See the 2026-09 hardening campaign section at the
> end of this file.

---

## Completed Fixes

### 1. Object Storage Data Loss Bug (CRITICAL) - FIXED

**Problem:** Object data was overwritten by metadata.

**Solution:** Separated object data and metadata paths:
- Object data: `<bucket>/<object>`
- Metadata: `<bucket>/.metadata/<object>.meta`

### 2. SigV4 Signature Calculation Issues - FIXED

**Problems fixed:**
- `host` header was not found because Go stores it in `r.Host`, not `r.Header`
- Added support for `STREAMING-UNSIGNED-PAYLOAD-TRAILER` (AWS CLI v2)
- Added `aws-chunked` Content-Encoding decoder

### 3. Empty Const Block - FIXED

Removed the empty const block.

### 4. ListObjectsV2 Nested Paths - FIXED

Changed from `os.ReadDir` to `filepath.WalkDir` for recursive metadata discovery.

### 5. deleteObjectHandler - FIXED

Now properly reads metadata, deletes object data, deletes metadata, and cleans up empty directories.

### 6. Inconsistent Error Responses - FIXED

All handlers now use `errorToXML()` with proper Content-Type headers.

### 7. Content-Length Handling - FIXED

Now uses `int64(len(body))` instead of `r.ContentLength`.

### 8. Multipart Storage Alignment - FIXED

Storage locations are now consistent between PUT and multipart upload handlers.

### 9. Bucket Name Validation - FIXED

Added `validateBucketName()` function enforcing S3 naming rules.

### 10. Object Key Validation - FIXED

Added `validateObjectKey()` function.

---

## Remaining Medium Priority Issues (Not Critical)

### 11. Race Conditions

**Problem:** No file locking. Concurrent operations on the same object could corrupt data.

**Status:** Not implemented - acceptable for single-user/development scenarios.

---

## Testing

### Unit Tests - COMPLETED

Created `main_test.go` with 17 passing tests:
- `TestHashSHA256`
- `TestHmacSHA256`
- `TestGetSigningKey`
- `TestErrorToXML`
- `TestParseInt`
- `TestGetEnvOrDefault`
- `TestValidateBucketName`
- `TestValidateObjectKey`
- `TestCreateBucketValidation`
- `TestObjectMetadataJSON`
- `TestListAllMyBucketsResultXML`
- `TestListBucketResultXML`
- `TestGetCanonicalURI`
- `TestGetCanonicalQueryString`
- `TestMultipartUploadJSON`
- `TestCleanupEmptyDirs`
- `TestCompleteMultipartUploadXML`

Run with: `go test -v ./...`

### Integration Test Script - COMPLETED

Created `scripts/test-s3-operations.sh` which tests:
- Create bucket
- List buckets
- Upload object
- List objects
- Download object
- Content integrity verification
- Head object
- Nested path objects
- Delete object
- Delete nested objects
- Delete bucket
- Optional: Multipart upload (set `RUN_MULTIPART_TEST=1`)

Run with: `./scripts/test-s3-operations.sh`

---

## Success Criteria - ALL MET

1. `aws s3 cp localfile s3://bucket/key` followed by `aws s3 cp s3://bucket/key localfile2` produces identical files
2. All standard aws-cli operations work without signature errors
3. Objects with nested paths (`folder/subfolder/file.txt`) work correctly
4. Multipart uploads produce valid, retrievable objects
5. All unit and integration tests pass

---

## 2026-09 Hardening Campaign

A dedicated bug-hunt audit was run against the codebase in September 2026. The
full findings - 12 HIGH, ~27 MED, and ~17 LOW severity - are documented in
`.hermes/audits/2026-09-27-bughunt-report.md` (not duplicated here).

All 12 HIGH findings are fixed, together with the MED/LOW items addressed on the
way. The campaign also landed meept-grade developer tooling (Makefile quality
gates, golangci-lint, gitleaks, git hooks) and a set of hardening features:

- **SigV4/auth hardening** (`sigv4.go`): strict canonical header/URI handling,
  region-mismatch rejection (`AuthorizationHeaderMalformed`/400), accepted
  STREAMING payload allowlist, `aws-chunked` decoding with
  `x-amz-decoded-content-length` verification (`VerifyDecodedLength`) wired into
  both PUT and UploadPart.
- **Storage atomicity** (`storage.go`): `writeFileAtomic` /
  `writeFileAtomicJSON` for all object-data and metadata JSON writes (temp file
  + fsync + rename), and `lockObject` per-key write serialization - superseding
  the "no file locking" note above. Multipart part files and complete are also
  atomic/locked.
- **Handler semantics** (`object_handlers.go`, `bucket_handlers.go`,
  `xml.go`): the data-loss bug (object data overwritten by metadata) confirmed
  fixed and regression-tested; actual file size served on GET with a warning on
  metadata mismatch; corrupt StoragePath falls back to the canonical path;
  bucket-level traversal names rejected (`validBucket`); object keys with `..`
  or `.metadata` segments rejected (`validateObjectKey`); every XML document
  carries the prolog and the pinned S3 namespace (`s3XMLNamespace`); real S3
  error codes/statuses; `encoding-type=url` support in ListObjectsV2; multipart
  complete adopts the Content-Type from initiate.
- **Actions hardening** (`actions.go`): variable values are shell-quoted on
  substitution (single-pass, no re-expansion), glob patterns compiled safely
  (no injection via patterns), action commands run under a 30s default timeout
  with the whole process group killed on expiry, and output capture truncated
  at 1MB per stream.
- **Server lifecycle** (`main.go`, `config.go`): explicit `http.Server` with
  read/write/idle timeouts, TLS 1.2 minimum version, graceful shutdown draining
  in-flight requests up to 30s on SIGINT/SIGTERM, and new config keys
  `listenAddr` / `certFile` / `keyFile` plus the `ZETAOBJECT_LISTEN_ADDR` env
  override. `make certs` generates certificates with SANs for localhost and
  127.0.0.1.
- **Tooling and honesty fixes**: Makefile gates (`lint`, `check`, `e2e`,
  `hooks`, `test-cover-enforce` with a 50% coverage floor), gitleaks secret
  scanning, git pre-commit/pre-push hooks, and honest exit codes in the test
  scripts.

The remaining known divergences from real S3 (keys with `..` segments rejected,
bucket `..` rejected, no Range support - GET serves full-body 200, single
credential, region pinned to `us-east-1`) are documented in `CLAUDE.md` and the
README. The next phase (leaves 3.1-3.6 of `docs/plans/hardening-2026-09/`)
adds Range/conditional requests, presigned URLs, multipart lifecycle APIs,
verified chunk signatures, CopyObject/DeleteObjects, and an e2e suite.
