# Protocol Conformance + Client Interop Plan — 2026-09-28

> Note: this tree predates the project rename; "mini-s3" in the text below is now "zeta-object".

Scope: gap items #3 (protocol conformance) and #4 (client interop matrix)
from the post-campaign test audit. Two leaves, then integration.

## Decisions (frozen)

- **Conformance suite**: vendor ceph/s3-tests (the industry-standard
  unofficial S3 compatibility suite; pytest/boto3, ~830 functional tests).
  Run it UNMODIFIED against mini-s3 and baseline the results — the pass/fail
  matrix IS the deliverable, plus fixes for any real bugs it finds in
  features mini-s3 claims (listed in README: buckets, objects, listing,
  multipart, copy, conditional, range, presigned).
  Out of scope by design (mini-s3 does not implement them): versioning,
  object lock, encryption/SSE, lifecycle, tagging, bucket policy, ACL
  enforcement, storage classes, CORS, website, s3select, logging, IAM/STS.
  These are deselected via pytest markers, not code changes.
- **Interop matrix**: drive the SAME scripted scenarios through 3 clients —
  aws-cli v2 (already the e2e driver), python boto3, and the mc client —
  and assert identical observable outcomes. New e2e cases under
  scripts/e2e/cases/ (12-interop-boto3.sh, 13-interop-mc.sh). Shared
  scenario table in scripts/e2e/interop-lib.sh so all clients run the same
  operations.
- Harness owns server lifecycle (reuse run-e2e.sh pattern). Conformance
  suite gets its own make target `make conformance` (clones ceph/s3-tests
  into a gitignored vendor dir, generates s3tests.conf, runs the marked
  subset, emits a results matrix doc). NOT in `make check`/CI gates —
  informational, run on demand.

## Leaves

- 5.1-conformance.md — ceph/s3-tests baseline: setup, config, run, results
  matrix, triage of failures into [real bug | documented divergence |
  out-of-scope feature], fixes for real bugs in claimed features.
- 5.2-interop.md — boto3 + mc client matrix over shared scenarios.

## Gate

Integration: make e2e still 101/101 (new cases add to the count),
make conformance runs to completion, results matrix committed under
docs/, coverage floor holds.

## Tracking

| Leaf | Status | Commit | Notes |
|---|---|---|---|
| 5.1 | COMPLETE | ddb609e | 277 in-scope; 4 real bugs fixed; matrix+ratchet committed |
| 5.2 | COMPLETE | 27524bc | boto3+mc 27/27 each; 155/155 e2e |
