# Capability Endpoints (?events / Version-Listing Extension) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md (docs/plans/metadata-zfs-2026-09/master.md)
- **Scope:** S3-frontend capability endpoints — object-level `GET /{bucket}/{key}?events`, bucket-level `GET /{bucket}?events`, and the derived version-listing extension `GET /{bucket}?events&versions` — SigV4-authed, following existing subresource dispatch and XML/JSON conventions.
- **Dependencies:** 01 (registry/attach), 02 (History/HistoryDetail — replay fixtures cover behavior); 03's parity suite must stay green after this leaf.
- **Estimated Context:** 55K
- **Concurrency Group:** B

## Goal

Expose provider event history through the existing S3 frontend as
subresource-style query parameters, exactly like the existing `?acl`,
`?uploads`, and `?uploadId` dispatch: authenticated with the same SigV4
path, dispatched from `bucketLevelDispatch`/`objectLevelDispatch` in
`main.go` BEFORE the method switch, erroring through `writeS3Error`, and —
after this leaf lands — still passing leaf 03's parity suite untouched
(capability endpoints must not alter core metadata responses).

Endpoint surface (designed here, pinned in master Contract 4):

| Request | Behavior |
|---|---|
| `GET /{bucket}/{key}?events` | JSON array of ObjectEvent for that key (plus loss info) |
| `GET /{bucket}?events` | Bucket-level JSON summary: dataset, record count, records_lost |
| `GET /{bucket}?events&versions` | Version-listing EXTENSION: XML listing derived from create/rename/truncate/remove events, marked non-standard and lossy |

## Context

mini-s3 routes through `rootHandler` (main.go:139) →
`bucketLevelDispatch`/`objectLevelDispatch`, where subresources are matched
via `r.URL.Query()` presence checks. Responses use `xml.Header` + pinned
namespace `s3XMLNamespace` (`xml.go:16`,
`http://s3.amazonaws.com/doc/2006-03-01/`) for XML documents; errors use
`writeS3Error(w, code, message, status)` (xml.go:62). Auth is SigV4
(single shared credential) enforced once in rootHandler before dispatch —
capability endpoints therefore inherit it automatically and MUST NOT add
their own auth path. Bucket name → filesystem path resolution (auto-
discovered vs config `buckets` map, symlinks followed) already exists in
the bucket handlers; reuse it.

Key files to understand before implementing:
- `main.go:140-260` - dispatch functions and the existing subresource checks
  to imitate.
- `xml.go` - s3XMLNamespace, writeS3Error, how ListBucketResult is marshaled.
- `types.go` - XML response struct conventions (xml tags, namespace pinning).
- `bucket_handlers.go` - bucket name → bucketPath resolution to reuse.
- Leaves 01/02 - `metadata.Lookup("zfs-events")`, `History`,
  `HistoryDetail`, `ProbeResult`.

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: capability_endpoints.go (package main)
package main

// handleObjectEvents serves GET /{bucket}/{key}?events (JSON).
func handleObjectEvents(w http.ResponseWriter, r *http.Request, bucketName, objectName string)

// handleBucketEvents serves GET /{bucket}?events and ?events&versions (JSON
// summary; XML version listing when "versions" is present).
func handleBucketEvents(w http.ResponseWriter, r *http.Request, bucketName string)

// XML response types for ?events&versions (types.go or this file — put them
// with the other XML structs per repo convention).
type ObjectEventHistory struct { /* JSON, below */ }

type ListObjectVersionsExt struct {
    XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListObjectVersionsExt"`
    Name     string   `xml:"Name"`
    IsLossy  bool     `xml:"IsLossy"`
    RecordsLost uint64 `xml:"RecordsLost"`
    Version  []ObjectVersionExt `xml:"Version"`
    // Continuation-style fields if truncation is implemented (leaf decision)
}

type ObjectVersionExt struct {
    Key          string `xml:"Key"`
    VersionId    string `xml:"VersionId"` // "txg-<txg>-obj-<object>" derived id
    IsLatest     bool   `xml:"IsLatest"`
    LastModified string `xml:"LastModified"` // S3 format; zero-time events: ""
    Size         int64  `xml:"Size"`
    Op           string `xml:"Op"` // originating event op: create|truncate|rename|remove
    IsDeleteMarker bool `xml:"IsDeleteMarker"` // remove events
}
```

### What This Leaf Consumes

```go
// From leaves 01/02 (package metadata):
//   Lookup(name) MetadataProvider; provider.History(ctx, bucketPath, key, q)
//   ([]ObjectEvent, error); ObjectEvent fields; HistoryDetail/RecordsLost.
// From package main (existing):
//   writeS3Error, s3XMLNamespace conventions, bucket path resolution.
```

### Wire contracts (from master Contracts 3/4)

- `?events` JSON responses: `Content-Type: application/json`; history
  objects serialize Op LOWERCASE and include a top-level lossy envelope:

```json
{
  "dataset": "tank/data",
  "recordsLost": 0,
  "events": [
    {"op": "create", "key": "a.txt", "txg": 1234, "timestamp": "0001-01-01T00:00:00Z", "sizeOld": 0, "sizeNew": 0}
  ]
}
```

  (`timestamp` may be zero — the upstream wire format does not emit `time`;
  DO NOT fake values. Document the field as best-effort.)
- `?events&versions`: XML with the pinned namespace, `IsLossy=true` +
  `RecordsLost>0` whenever the ring buffer reports loss (master decision 4).
- No provider attached for the bucket → `503` `writeS3Error(w,
  "NotImplemented", "no metadata provider available for this bucket",
  http.StatusServiceUnavailable)`; unknown bucket → existing 404 path.
- Errors from History exec failures → `writeS3Error(w, "InternalError",
  ..., 500)` with NO zfs stderr leaked to the client (log server-side).

## Tasks

### Task 1: Dispatch wiring (subresource checks, existing style)

**Objective:** `?events` and `?events&versions` reach the new handlers from
both dispatch functions, after auth, before the method switch.

**Files:**
- Modify: `main.go` (objectLevelDispatch and bucketLevelDispatch)
- Test: `capability_endpoints_test.go`

**Step 1: Write failing test**

```go
func TestEventsDispatchRoutes(t *testing.T) {
    // 404 unknown bucket takes precedence; provider-missing bucket gets 503;
    // the routing itself must not 405/404 on the query param.
    rec := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/no-such-bucket/some/key?events", nil)
    rootHandler(rec, req)
    if rec.Code != http.StatusNotFound {
        t.Fatalf("unknown bucket?events = %d, want 404", rec.Code)
    }

    rec = httptest.NewRecorder()
    req = httptest.NewRequest("GET", "/plainbucket?events", nil)
    rootHandler(rec, req)
    // plainbucket exists (no provider) -> 503 NotImplemented per contract
    if rec.Code != http.StatusServiceUnavailable {
        t.Fatalf("no-provider ?events = %d, want 503", rec.Code)
    }
}
```

(Adapt bucket setup to the repo's existing test fixtures — main_test.go
shows how buckets/dataDir are provisioned per test. The assertions above
describe the target behavior; wire the fixtures accordingly.)

**Step 2: Run test to verify failure**

Run: `go test . -run TestEventsDispatchRoutes -count=1 -v`
Expected: FAIL - ?events not routed (falls through to list/GET behavior)

**Step 3: Write minimal implementation**

In `objectLevelDispatch`, after the multipart checks and before the method
switch:

```go
// MetadataProvider capability: object event history (?events).
if _, ok := r.URL.Query()["events"]; ok && r.Method == "GET" {
    handleObjectEvents(w, r, bucketName, objectName)
    return
}
```

In `bucketLevelDispatch`, same position relative to `?location`/`?list-type`:

```go
// MetadataProvider capability: bucket event summary / version listing.
if _, ok := r.URL.Query()["events"]; ok && r.Method == "GET" {
    handleBucketEvents(w, r, bucketName)
    return
}
```

**Step 4: Run test to verify pass**

Run: `go test . -run TestEventsDispatchRoutes -count=1 -v`
Expected: PASS

### Task 2: Provider resolution + 503 path

**Objective:** Handlers resolve the attached provider for the bucket
(via the attach map built at startup from `ProbeAndAttach` — see leaf 01's
wiring; until backend-interface lands, the test seam registers a fake and
the handler resolves via `metadata.Lookup`), returning the contracted 503
when absent, and resolving bucketName → bucketPath with the existing
bucket-path helper.

**Files:**
- Create: `capability_endpoints.go`
- Modify: `capability_endpoints_test.go`

**Step 1: Write failing test**

```go
type stubEventsProvider struct {
    events []metadata.ObjectEvent
    detail metadata.HistoryDetail
    err    error
}
func (s *stubEventsProvider) Name() string { return "stub" }
func (s *stubEventsProvider) Probe(ctx context.Context, p string) (metadata.ProbeResult, error) {
    return metadata.ProbeResult{Available: true, Dataset: "stub/data"}, nil
}
func (s *stubEventsProvider) History(ctx context.Context, bucketPath, key string, q metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
    return s.events, s.err
}
func (s *stubEventsProvider) Purge(ctx context.Context, p string) error { return nil }

func TestObjectEventsHandler(t *testing.T) {
    metadata.Register(&stubEventsProvider{
        events: []metadata.ObjectEvent{
            {Op: "create", Key: "a.txt", Txg: 1234},
        },
        detail: metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
    })
    // ... provision bucket dir + provider-attachment seam, then:
    rec := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/b/a.txt?events", nil)
    handleObjectEvents(rec, req, "b", "a.txt")
    if rec.Code != http.StatusOK {
        t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
    }
    if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
        t.Fatalf("content-type %q", ct)
    }
    var resp ObjectEventHistory
    if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
        t.Fatal(err)
    }
    if resp.Dataset != "stub/data" || len(resp.Events) != 1 || resp.Events[0].Op != "create" {
        t.Fatalf("resp = %+v", resp)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test . -run TestObjectEventsHandler -count=1 -v`
Expected: FAIL - handler/types undefined

**Step 3: Write minimal implementation**

```go
// ObjectEventHistory is the JSON envelope for ?events responses.
type ObjectEventHistory struct {
    Dataset     string                   `json:"dataset"`
    RecordsLost uint64                   `json:"recordsLost"`
    Events      []objectEventJSON        `json:"events"`
}

type objectEventJSON struct {
    Op        string `json:"op"`
    Key       string `json:"key,omitempty"`
    OldKey    string `json:"oldKey,omitempty"`
    Txg       uint64 `json:"txg"`
    Timestamp string `json:"timestamp"` // RFC3339; zero-time = "0001-01-01T00:00:00Z"
    SizeOld   int64  `json:"sizeOld,omitempty"`
    SizeNew   int64  `json:"sizeNew,omitempty"`
    UID       uint32 `json:"uid,omitempty"`
    GID       uint32 `json:"gid,omitempty"`
}

// providerForBucket resolves the attached MetadataProvider for a bucket and
// writes the contracted 503 when none is attached. Returns nil after
// writing the error.
func providerForBucket(w http.ResponseWriter, bucketName string) metadata.MetadataProvider {
    p := metadata.Lookup("zfs-events") // attach map wiring: see Notes
    if p == nil {
        writeS3Error(w, "NotImplemented", "no metadata provider available for this bucket", http.StatusServiceUnavailable)
        return nil
    }
    return p
}
```

**Step 4: Run test to verify pass**

Run: `go test . -run TestObjectEventsHandler -count=1 -v`
Expected: PASS

### Task 3: Object + bucket handlers (JSON)

**Objective:** `handleObjectEvents` (key-scoped History) and
`handleBucketEvents` (dataset summary) per the wire contract, with exec
failures mapped to InternalError without leaking stderr.

**Files:**
- Modify: `capability_endpoints.go`
- Test: `capability_endpoints_test.go`

**Step 1: Write failing test**

```go
func TestObjectEventsExecFailureNoStderrLeak(t *testing.T) {
    metadata.Register(&stubEventsProvider{err: errors.New("exit status 1: cannot get events for 'tank': secrets-here")})
    rec := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/b/a.txt?events", nil)
    handleObjectEvents(rec, req, "b", "a.txt")
    if rec.Code != http.StatusInternalServerError {
        t.Fatalf("status %d, want 500", rec.Code)
    }
    if strings.Contains(rec.Body.String(), "secrets-here") {
        t.Fatal("provider error detail leaked to client")
    }
}

func TestBucketEventsSummary(t *testing.T) {
    metadata.Register(&stubEventsProvider{
        events: []metadata.ObjectEvent{{Op: "create", Key: "a", Txg: 1}},
        detail: metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 3},
    })
    rec := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/b?events", nil)
    handleBucketEvents(rec, req, "b")
    var resp ObjectEventHistory
    if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
        t.Fatal(err)
    }
    if !respLossyFalse && resp.RecordsLost != 3 { // RecordsLost surfaced per master decision 4
        t.Fatalf("recordsLost = %d, want 3", resp.RecordsLost)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test . -run 'TestObjectEventsExecFailure|TestBucketEventsSummary' -count=1 -v`
Expected: FAIL

**Step 3: Write minimal implementation**

```go
func handleObjectEvents(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
    p := providerForBucket(w, bucketName)
    if p == nil {
        return
    }
    bucketPath, ok := bucketPathFor(w, bucketName) // existing bucket->path helper
    if !ok {
        return
    }
    // max-events query param caps HistoryQuery.MaxEvents (default 1000).
    q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
    events, err := p.History(r.Context(), bucketPath, objectName, q)
    if err != nil {
        log.Printf("metadata: events for %s/%s: %v", bucketName, objectName, err)
        writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
        return
    }
    writeJSON(w, http.StatusOK, ObjectEventHistory{
        Dataset:     lastDetail.Dataset,
        RecordsLost: lastDetail.RecordsLost,
        Events:      toEventJSON(events),
    })
}
```

(`handleBucketEvents` follows: empty key → full event stream summarized;
`?versions` present → Task 4's XML listing. `writeJSON` marshals with
`json.MarshalIndent` or compact — match whichever convention exists in the
repo; if none, compact + `application/json`.)

**Step 4: Run test to verify pass**

Run: `go test . -run 'TestObjectEvents|TestBucketEvents' -count=1 -v`
Expected: PASS

### Task 4: Version-listing extension (?events&versions, XML)

**Objective:** Derive a version listing from events: create/rename-to →
new version; truncate → new version with new_size; remove → delete marker.
Newest first (ring buffer order). Mark lossy per master decision 4.

**Files:**
- Modify: `capability_endpoints.go`
- Modify: `capability_endpoints_test.go`

**Step 1: Write failing test**

```go
func TestVersionsExtensionXML(t *testing.T) {
    metadata.Register(&stubEventsProvider{
        events: []metadata.ObjectEvent{
            {Op: "truncate", Key: "a.txt", Txg: 30, SizeNew: 300},
            {Op: "create", Key: "a.txt", Txg: 10, SizeNew: 100},
            {Op: "remove", Key: "gone.txt", Txg: 20},
        },
        detail: metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 2},
    })
    rec := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/b?events&versions", nil)
    handleBucketEvents(rec, req, "b")

    if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
        t.Fatalf("content-type %q, want xml", ct)
    }
    var got ListObjectVersionsExt
    if err := xml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
        t.Fatal(err)
    }
    if !got.IsLossy || got.RecordsLost != 2 {
        t.Fatalf("loss flag: %+v", got)
    }
    var aVersions []ObjectVersionExt
    for _, v := range got.Version {
        if v.Key == "a.txt" {
            aVersions = append(aVersions, v)
        }
    }
    if len(aVersions) != 2 || aVersions[0].VersionId == aVersions[1].VersionId {
        t.Fatalf("a.txt versions = %+v", aVersions)
    }
    if aVersions[0].Size != 300 || aVersions[0].IsLatest != true {
        t.Fatalf("newest first broken: %+v", aVersions[0])
    }
    var gone ObjectVersionExt
    for _, v := range got.Version {
        if v.Key == "gone.txt" {
            gone = v
        }
    }
    if !gone.IsDeleteMarker {
        t.Fatalf("remove not marked: %+v", gone)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test . -run TestVersionsExtensionXML -count=1 -v`
Expected: FAIL

**Step 3: Write minimal implementation**

Derivation rules (PINNED — document them in the response via a comment and
the leaf report):
- Iterate events newest-first (provider ring-buffer order).
- `create`, `rename` (to Key), `truncate` → version entry
  `{Key, VersionId: "txg-<txg>-obj-<n>"? (txg alone is not unique — use
  "txg-<txg>-<key-hash8>" or event index; PIN one and test it)`,
  Size: SizeNew, IsLatest: first entry per key, IsDeleteMarker: false}.
- `remove` → `{Key, IsDeleteMarker: true, IsLatest: first per key}`.
- `link`/`symlink`/`setattr` → no version entry (setattr MAY update the
  prior entry's size if SizeNew>0 — keep v1 simple: skip).
- `IsLossy`/`RecordsLost` from HistoryDetail whenever > 0.
- `LastModified`: empty string under the current wire format (no `time`
  field); the XML field stays so the upstream print_event extension
  populates it without an API change.

```go
func versionsFromEvents(events []metadata.ObjectEvent, lost uint64) ListObjectVersionsExt {
    out := ListObjectVersionsExt{IsLossy: lost > 0, RecordsLost: lost}
    latest := map[string]bool{}
    for i, e := range events { // newest-first
        v := ObjectVersionExt{Key: e.Key}
        switch e.Op {
        case "create", "rename", "truncate":
            v.VersionId = fmt.Sprintf("txg-%d-%d", e.Txg, i) // pinned: txg + stream index
            v.Size = e.SizeNew
            v.Op = e.Op
        case "remove":
            v.IsDeleteMarker = true
            v.VersionId = fmt.Sprintf("txg-%d-%d", e.Txg, i)
            v.Op = e.Op
        default:
            continue // link/symlink/setattr: no version entry in v1
        }
        v.IsLatest = !latest[e.Key]
        latest[e.Key] = true
        out.Version = append(out.Version, v)
    }
    return out
}
```

Marshal with `xml.Header` + the pinned namespace struct tags; Content-Type
`application/xml` matching existing XML responses.

**Step 4: Run test to verify pass**

Run: `go test . -run TestVersionsExtensionXML -count=1 -v`
Expected: PASS

### Task 5: Query params, parity non-regression, skip-graceful integration

**Objective:** `max-events` param handling; re-run of the parity suite
proving zero core-metadata drift; an httptest-level check that ?events on a
provider-attached bucket coexists with normal GET.

**Files:**
- Modify: `capability_endpoints_test.go`

**Step 1: Write failing test**

```go
func TestEventsDoNotDisturbCoreResponses(t *testing.T) {
    // Provision provider-attached bucket with an object; then:
    // 1. GET /b/a.txt (no ?events) -> normal 200 object response, identical
    //    headers to a plain bucket's response for the same content
    //    (the leaf 03 parity invariant, asserted here at handler level).
    // 2. GET /b/a.txt?events -> 200 JSON (capability).
    // 3. GET /b/a.txt again -> identical to step 1 (no state mutated).
    // 4. ?events&max-events=1 caps the array length.
}
```

**Step 2: Run test to verify failure**

Run: `go test . -run TestEventsDoNotDisturbCoreResponses -count=1 -v`
Expected: FAIL - max-events not honored (other steps pass after Tasks 1-3)

**Step 3: Write minimal implementation**

```go
func maxEventsFromQuery(r *http.Request) int {
    s := r.URL.Query().Get("max-events")
    if s == "" {
        return 1000
    }
    n, err := strconv.Atoi(s)
    if err != nil || n <= 0 {
        return 1000
    }
    if n > 10000 {
        n = 10000
    }
    return n
}
```

**Step 4: Run test to verify pass + full gates**

Run:
- `go test . -count=1 -v` (whole package-main suite green)
- `go test ./internal/metadata/ -run TestParity -count=1 -v` (leaf 03 gate still green)
- `make test-race` green

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing
- [ ] Dispatch mirrors existing subresource style (query-presence check, after auth, before method switch)
- [ ] SigV4 inherited from rootHandler — endpoints add NO auth code of their own
- [ ] 503 NotImplemented when no provider; 404 unknown bucket; 500 InternalError with NO stderr/detail leak
- [ ] JSON contract: lowercase op, recordsLost always present, zero timestamps NOT fabricated
- [ ] Version extension: newest-first, IsLatest per key, delete markers, IsLossy/RecordsLost per master decision 4, pinned VersionId scheme
- [ ] XML carries pinned s3 namespace; Content-Type application/xml
- [ ] Parity suite (leaf 03) still green; core GET/HEAD responses unchanged with provider attached
- [ ] max-events honored, bounded (1..10000, default 1000)
- [ ] gofmt clean; stdlib only; no line-number corruption
- [ ] No scope creep: no Purge endpoint, no snapshot support, no per-key inode `-o` fast path

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing
- [ ] Wire contract matches master Contract 4 (status codes, content types, JSON/XML shapes)
- [ ] Dispatch placement matches existing subresource pattern (compare ?acl/?uploads diffs)
- [ ] No client-visible error strings contain zfs/exec detail
- [ ] VersionId derivation is pinned, deterministic, and tested
- [ ] Code follows project conventions (stdlib, errors as values, table-driven tests)
- [ ] No bugs, no security issues (auth inherited, no path traversal via key params — History filtering is string equality only)
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- **Auth:** rootHandler authenticates BEFORE dispatch, so ?events is
  SigV4-protected by construction. Do not add per-handler auth checks; DO
  add a routing test asserting unsigned requests are rejected if the repo's
  test utilities make that cheap (sigv4 negative tests exist).
- **Provider resolution today vs after backend-interface:** the durable
  design is a per-bucket attach map built at startup
  (`ProbeAndAttach` per bucket → `CapabilitySet.MetadataProviders`), owned
  by the Backend seam tree. Until that lands, this leaf resolves via
  `metadata.Lookup` with a package-level attach-map shim
  (`attachProvidersFor(bucketPath)`) that backend-interface can replace.
  Keep the shim behind one function so the swap is one file.
- **VersionId stability:** derived ids (`txg-<txg>-<index>`) are stable only
  within one History response (ring buffers lose records; indices shift
  across calls). This is WHY the response must carry IsLossy — clients
  cannot treat VersionIds as durable handles. Document this in any
  user-facing docs; do not pretend S3 version semantics.
- **Timestamp honesty:** under the current upstream wire format, events
  carry no wall-clock time. The JSON `timestamp` field will serialize as
  zero-time and XML LastModified as empty. The endpoint must NOT order by
  timestamp — order by ring-buffer stream order (newest-first), which is
  the only reliable ordering. If upstream later emits `time`, the parser
  (leaf 02) already maps it; endpoints then gain real timestamps with zero
  API change.
- **bucketPath resolution:** reuse the existing bucket-name → path helper
  (bucket_handlers.go). Custom-path buckets from config `buckets` work
  automatically; symlink resolution happens in Probe (leaf 01), so the
  dataset discovered matches the real mount.
