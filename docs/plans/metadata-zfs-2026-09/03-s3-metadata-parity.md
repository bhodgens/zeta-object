# S3 Metadata Parity Gate - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md (docs/plans/metadata-zfs-2026-09/master.md)
- **Scope:** The enforcement leaf: parity test suite proving a provider-enabled bucket returns byte-identical responses to a plain-FS bucket, ObjectMetadata sidecar byte-compat, and a `make parity-test` CI target. This leaf ENFORCES parity; it does not implement features.
- **Dependencies:** 01-provider-interface-probe.md (registry + ProbeAndAttach + fake-provider pattern); consumes the real provider only as a black box.
- **Estimated Context:** 45K
- **Concurrency Group:** B

## Goal

Build and wire the parity gate that makes the CRITICAL user requirement
testable forever: for an identical request matrix, a bucket with the
zfs-events provider attached (fake provider is sufficient — parity is about
the core metadata path being untouched, not about ZFS) and a plain-FS
bucket MUST produce identical response headers and bodies; and the
`.metadata/*.meta` JSON sidecars must remain byte-compatible modulo
StoragePath. The gate ships as (1) a Go test suite
`internal/metadata/parity_test.go`, (2) a `make parity-test` target, and
(3) inclusion in `make precommit` so no future provider or backend change
can silently drift S3 metadata semantics.

## Context

The canonical S3 metadata surface (owned by tree `object-model-2026-09`,
leaf 02) is exactly: Content-Type header, Content-Length, ETag,
Last-Modified, and x-amz-meta-* headers. Current implementation:
`ObjectMetadata` (types.go:48) persisted as JSON under `.metadata/`, written
via `storage.go` atomic helpers, surfaced by `object_handlers.go`
(putObjectHandler builds it at :129, headObjectHandler/getObjectHandler
serve it). The provider seam must be invisible on this surface: attaching a
provider changes NOTHING about Get/Head/Put/List.

zeta-object's existing test style: package-main `_test.go` files, stdlib
testing (no testify), httptest against the real rootHandler, and an
integration harness under `scripts/`/e2e as the regression gate. Follow it.

Key files to understand before implementing:
- `types.go:48` - ObjectMetadata: the thing that must stay byte-compatible.
- `object_handlers.go` - put/get/head handlers; `resolveObjectDataPath`
  (:173); copy metadata (:1074).
- `storage.go` - `writeFileAtomicJSON` — how sidecars are serialized (this
  defines "byte-compatible").
- `main.go:139` - rootHandler: how you will drive requests in tests.
- `Makefile` - where the parity-test target and precommit chain live.
- `01-provider-interface-probe.md` - Register/ProbeAndAttach + fakeProvider.

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/metadata/parity_test.go (test-only export surface)
// The parity suite is a test, not an API. Its stable outputs are:
//   - TestParity... test names referenced by the Makefile target
//   - make parity-test  (runs the suite)
// Makefile addition (owner: this leaf):
//
// parity-test:
// 	go test ./internal/metadata/ -run 'TestParity' -count=1 -v
//
// precommit: ...existing targets... parity-test
```

### What This Leaf Consumes

```go
// From leaf 01:
package metadata // Register, Lookup, ProbeAndAttach, MetadataProvider,
                 // ProbeResult, HistoryQuery, ObjectEvent
// From package main (existing, read-only):
//   rootHandler, ObjectMetadata, storage atomic helpers
```

## Tasks

### Task 1: Parity harness — two buckets, one request matrix

**Objective:** A test harness that creates a plain-FS bucket and a
provider-attached bucket in one server, replays an identical request matrix
through the real rootHandler, and captures full responses (status + all
headers + body) for diffing.

**Files:**
- Create: `internal/metadata/parity_test.go`
- Test: `internal/metadata/parity_test.go`

**Step 1: Write failing test**

```go
package metadata

import (
    "bytes"
    "io"
    nethttp "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "testing"
)

// capturedResponse is everything S3 metadata parity cares about.
type capturedResponse struct {
    Status  int
    Headers nethttp.Header
    Body    []byte
}

// parityHarness runs an identical request matrix against two bucket roots
// (plain vs provider-attached) and returns responses keyed by request label.
type parityHarness struct {
    t *testing.T
}

func (h *parityHarness) run(matrix func(bucket string) []*nethttp.Request) map[string]capturedResponse {
    h.t.Helper()
    out := map[string]capturedResponse{}
    for _, variant := range []struct {
        name     string
        register bool
    }{{"plain", false}, {"provider", true}} {
        root := h.t.TempDir()
        bucketDir := filepath.Join(root, "par")
        if err := os.MkdirAll(bucketDir, 0o755); err != nil {
            h.t.Fatal(err)
        }
        if variant.register {
            Register(&parityFakeProvider{name: "parity-fake"})
            // The attach point under test: provider present must not change
            // handler behavior. (ProbeAndAttach wiring is leaf 01; here we
            // assert handler-level parity regardless.)
            _ = ProbeAndAttach(context.Background(), bucketDir)
        }
        // Drive the real rootHandler (package main) via httptest — see
        // existing main_test.go for the established pattern; adapt, do not
        // reinvent (handler construction may need a small unexported test
        // helper in package main; prefer reusing whatever main_test.go does).
        for label, req := range labeledMatrix(h.t, matrix(bucketDirToBucket(variant.name))) {
            rec := httptest.NewRecorder()
            rootHandlerForTest(rec, req) // same entry point as production
            body, _ := io.ReadAll(rec.Body)
            out[variant.name+"/"+label] = capturedResponse{
                Status: rec.Code, Headers: rec.Header(), Body: body,
            }
        }
    }
    return out
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestParity -v`
Expected: FAIL - harness helpers undefined (and package-metadata tests
cannot yet reach package-main's rootHandler — see Task 2's seam decision)

**Step 3: Write minimal implementation**

The harness needs a way to invoke the production handler from package
metadata. Resolve the seam FIRST, in this order of preference:
1. If rootHandler and config wiring are already exported or test-exported
   in package main and the test can live in package main
   (`metadata_parity_test.go` in repo root, `package main`) — DO THAT;
   simplest, zero seams.
2. Otherwise add a tiny export_test.go-style bridge in package main.
The harness compares, per label: Status equal; every header key present in
both with equal values EXCEPT `Date` (per-response) and `Last-Modified`
(compare within the mtime tolerance policy of Task 3); bodies byte-equal.

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestParity -v` (or the package-main
path you chose)
Expected: PASS on the empty matrix (harness works, nothing diffed yet)

### Task 2: The parity matrix

**Objective:** Define the request matrix that locks the canonical surface:
simple PUT, PUT with x-amz-meta-*, GET, HEAD, ListObjectsV2, multipart
init+complete, COPY — each with provider attached and not.

**Files:**
- Modify: `internal/metadata/parity_test.go`

**Step 1: Write failing test**

```go
func TestParityIdenticalResponses(t *testing.T) {
    h := &parityHarness{t: t}
    resp := h.run(func(bucket string) []*nethttp.Request {
        return []*nethttp.Request{
            sigV4Put(t, bucket, "a.txt", "text/plain", nil, []byte("hello")),
            sigV4Put(t, bucket, "meta.txt", "app/x", map[string]string{"team": "core"}, []byte("m")),
            sigV4Get(t, bucket, "a.txt"),
            sigV4Head(t, bucket, "a.txt"),
            sigV4Get(t, bucket, "meta.txt"),
            sigV4ListV2(t, bucket, ""),
            sigV4Copy(t, bucket, "a.txt", "copy.txt"),
            // multipart: initiate + uploadPart + complete for "big.bin"
            sigV4Multipart(t, bucket, "big.bin", []byte("0123456789")),
        }
    })
    diffParityResponses(t, resp)
}
```

**Step 2: Run test to verify failure**

Run: `go test ... -run TestParityIdenticalResponses -v`
Expected: FAIL - sigV4* helpers undefined

**Step 3: Write minimal implementation**

Write the sigV4* helpers by REUSING the existing signing test utilities in
the repo (sigv4_presigned_test.go / main_test.go already sign requests with
the default zetaadmin credentials — copy that pattern). Request bodies and
metadata must be IDENTICAL across the two variants so any diff is a real
parity break. `diffParityResponses` fails with a precise report: label,
which header/body differed, both values.

**Step 4: Run test to verify pass**

Run: `go test ... -run TestParityIdenticalResponses -v`
Expected: PASS — today's handlers are provider-agnostic, so the suite must
be green on arrival. If it is NOT green, you have found a real coupling bug:
STOP, report it in the leaf report, do not paper over it by loosening the
comparison.

### Task 3: Sidecar byte-compat + mtime tolerance policy

**Objective:** Assert `.metadata/*.meta` JSON is byte-identical between
variants (modulo StoragePath), and pin the Last-Modified comparison policy.

**Files:**
- Modify: `internal/metadata/parity_test.go`

**Step 1: Write failing test**

```go
func TestParitySidecarByteCompat(t *testing.T) {
    // After running the Task 2 matrix against both variants, read every
    // .metadata/*.meta from both bucket roots and compare pairwise
    // (same relative filename): byte-equal JSON EXCEPT the storagePath
    // field, which legitimately differs by root. Compare by unmarshaling
    // into a map, deleting storagePath, re-marshaling deterministically.
}
```

**Step 2: Run test to verify failure**

Run: `go test ... -run TestParitySidecarByteCompat -v`
Expected: FAIL - helper undefined

**Step 3: Write minimal implementation**

```go
// sidecarComparable returns the sidecar JSON with storagePath removed.
func sidecarComparable(t *testing.T, raw []byte) []byte {
    t.Helper()
    var m map[string]any
    if err := json.Unmarshal(raw, &m); err != nil {
        t.Fatalf("sidecar not valid JSON: %v", err)
    }
    delete(m, "storagePath")
    out, err := json.Marshal(m)
    if err != nil {
        t.Fatal(err)
    }
    return out
}
```

**Last-Modified tolerance policy (PINNED):** Last-Modified derives from the
file's mtime. The harness writes identical content to both buckets within
the same test second, so mtimes can differ by seconds of test execution.
Policy: Last-Modified values must parse as HTTP-date and be within 60
seconds of each other; ALL other headers must be exactly equal. If a future
change makes them exactly equal, tighten this — never loosen.

**Step 4: Run test to verify pass**

Run: `go test ... -run TestParity -v`
Expected: PASS

### Task 4: make parity-test target + precommit wiring

**Objective:** CI enforcement so the gate runs on every precommit.

**Files:**
- Modify: `Makefile`

**Step 1: Write failing test**

Run: `make parity-test`
Expected: FAIL - No rule to make target (before this task)

**Step 2: Confirm old state**

`grep -n parity-test Makefile` returns nothing.

**Step 3: Write minimal implementation**

```make
parity-test: ## Run the S3 metadata parity gate (FS vs provider-enabled)
	go test ./internal/metadata/ -run 'TestParity' -count=1 -v
```

and append `parity-test` to the `precommit` target's dependency chain
(after `test`, before `mod-tidy-check`, matching the existing style).

**Step 4: Run test to verify pass**

Run: `make parity-test` — PASS. Run `make precommit` — full chain green
(or at minimum: parity-test runs and passes; other pre-existing targets
unchanged).

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented; `make parity-test` green
- [ ] Parity matrix covers: simple PUT, PUT with x-amz-meta-*, GET, HEAD, ListObjectsV2, COPY, multipart completion
- [ ] Comparison: status + ALL headers (Date excluded per-response; Last-Modified within pinned 60s tolerance) + byte-equal bodies
- [ ] Sidecar byte-compat modulo storagePath verified
- [ ] Suite is green ON ARRIVAL — if not, the finding is REPORTED, not masked
- [ ] `make precommit` chain includes parity-test and still runs
- [ ] Suite passes with NO zfs binary present (fake provider only)
- [ ] gofmt clean; no line-number corruption; stdlib only
- [ ] No scope creep: no endpoint handlers, no provider features — ENFORCEMENT ONLY

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] The parity suite actually drives the PRODUCTION rootHandler (not a copy)
- [ ] Matrix completeness per Task 2 list
- [ ] Tolerance policy implemented exactly as pinned (only Last-Modified, 60s, HTTP-date parse)
- [ ] Makefile target + precommit wiring present and correct
- [ ] A deliberately-broken comparison (e.g. temporarily assert wrong header) FAILS the suite — spot-check the gate has teeth
- [ ] No bugs, no flaky timing (60s tolerance is generous; suite must not sleep/poll)
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- **This leaf enforces; it does not implement.** If the suite fails on
  arrival, that is a FINDING (the provider seam or endpoints leak into core
  metadata) — report it to the orchestrator; never weaken an assertion to
  get green.
- **Why a fake provider suffices:** parity is a property of the CORE
  metadata path's independence from provider attachment. The real
  zfs-events provider is exercised (via replay fixture) in leaf 02 and the
  endpoints in leaf 04; re-running the parity matrix with the real provider
  would add ZFS-host requirements to CI for no extra protection of the
  canonical surface.
- **Test placement:** prefer `package main` test file
  (`metadata_parity_test.go` at repo root) if that is what existing
  handler tests use — follow the repo's established pattern over the
  internal/metadata path if the seam demands it; the Makefile -run pattern
  `TestParity` matches either location. Report which you chose.
- The 50% coverage floor (`make test-cover-enforce`) is unaffected: this
  leaf adds tests only plus a Makefile target.
