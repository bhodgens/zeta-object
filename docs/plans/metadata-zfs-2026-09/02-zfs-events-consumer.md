# zfs-events Consumer - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md (docs/plans/metadata-zfs-2026-09/master.md)
- **Scope:** The concrete `zfs-events` MetadataProvider: exec wrapper for `zfs events -j`, strict JSON parser for the pinned wire format, field mapping into ObjectEvent, records_lost propagation, Purge, and record-and-replay fixture tests.
- **Dependencies:** Contract 1 + Contract 3 from the master (frozen, inlined below). Leaf 01's files are concurrency-group siblings — implement against the inlined contracts, not against 01's working tree.
- **Estimated Context:** 55K
- **Concurrency Group:** A

## Goal

Implement the `zfs-events` provider in package `internal/metadata`: it
probes a bucket path (DetectZFS + dataset resolution + `org.openzfs:events`
feature check), serves `History` by executing `zfs events -j <dataset>` and
parsing the JSON array into `[]ObjectEvent`, reports lossiness
(`records_lost`), and implements `Purge` via `zfs events -c <dataset>`.
The parser MUST handle the EXACT wire format of the branch's `print_event`
implementation — verified against
`/Users/caimlas/git/zfs-metadata/cmd/zfs/zfs_main.c:8332` — which differs
from the feature brief (uppercase ops, limited keys, plaintext trailing
loss line; see Interface Contracts). All ZFS-dependent behavior is testable
on macOS via a replay fixture: `exec.Command` is wrapped behind an
unexported seam so tests run a stub script instead of the real binary.

## Context

zeta-object (module `zeta-object`, Go 1.25, stdlib only) is adding an optional
metadata-enrichment seam (master Contract 1). This leaf builds the first
concrete provider against the user's OpenZFS branch
(`/Users/caimlas/git/zfs-metadata`, branch `extended-metadata`), which adds
per-dataset ring-buffer file-event logs exposed through the zfs CLI:

```
zfs events [-c] [-j] [-n max] [-o object] <dataset>
```

- `-j` prints a JSON array (compact, one object per event).
- `-c` clears the ring buffer (Purge).
- `-n max` limits output.
- `-o <object-id>` filters to one file's object id (inode number on Linux —
  see zfs_main.c: the filter is `stat().st_ino`).
- Feature gate: dataset property `events=on`; pool feature
  `org.openzfs:events`.
- Ring buffer wraparound drops old records; the CLI reports the count.

Key files to understand before implementing:
- `internal/metadata/metadata.go` - frozen interface + ObjectEvent (leaf 01).
- `internal/metadata/fsdetect*.go`, `internal/metadata/zfs_cmd.go` - detection
  + `ResolveDataset` (leaf 01).
- `/Users/caimlas/git/zfs-metadata/cmd/zfs/zfs_main.c` lines 8283-8600 -
  `zfs_event_op_name`, `print_event`, `zfs_do_events`: THE ground truth for
  parsing. Read with terminal grep/sed, never read_file.
- `/Users/caimlas/git/zfs-metadata/include/sys/zfs_events.h:86-99` - kernel
  nvlist field names (`time`, `mode`, `attrs`, `uid`, `gid` exist there but
  are NOT currently printed as JSON).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/metadata/zfs_events.go
package metadata

// NewZFSEventsProvider returns the "zfs-events" provider.
func NewZFSEventsProvider() MetadataProvider

// HistoryDetail carries information ObjectEvent cannot: lossiness of the
// ring buffer and the source dataset. History returns it via the package
// detail hook so endpoints (leaf 04) can surface it.
func LastHistoryDetail() HistoryDetail

type HistoryDetail struct {
    Dataset     string
    RecordsLost uint64 // nonzero = history is lossy; surfaces records_lost
}
```

(If a cleaner channel for detail fits — e.g. `HistoryDetail(ctx, ...)`
method added to the concrete type, not the frozen interface — prefer that;
the frozen interface MUST NOT gain methods. `LastHistoryDetail` is a
per-call accessor only because the frozen interface's History signature is
fixed; document its non-concurrency if you keep it, or store detail keyed by
the request context.)

### What This Leaf Consumes

```go
// From leaf 01 (inlined frozen contracts — implement to these; if 01 has
// already landed in your working tree, use its real files):
type MetadataProvider interface { /* Name/Probe/History/Purge as frozen */ }
type ProbeResult struct{ Available bool; Reason string; Dataset string }
type HistoryQuery struct{ MaxEvents int; Since time.Time }
type ObjectEvent struct {
    Op string; Key, OldKey string; Timestamp time.Time; Txg uint64
    SizeOld, SizeNew int64; UID, GID uint32
}
func DetectZFS(path string) (bool, error)
func ResolveDataset(ctx context.Context, path string) (string, error)
```

### Contract 3 (from master): the pinned wire format

```
[{"txg":1234,"object":256,"op":"CREATE","name":"report.pdf"},
 {"txg":1240,"object":256,"op":"TRUNCATE","name":"report.pdf","old_size":1024,"new_size":2048},
 {"txg":1251,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","old_parent":2},
 {"txg":1255,"object":301,"op":"SYMLINK","name":"link","target":"a.txt"}]
N record(s) lost to log wraparound        <- OPTIONAL plaintext line AFTER ]
```

- Ops UPPERCASE on the wire; lowercase into ObjectEvent.Op. Unknown ops map
  to the lowercase op string verbatim (e.g. future "CLONE" -> "clone") —
  never error.
- Always: `txg`, `object`, `op`. Optional: `name`, `parent` (omitted at 0);
  RENAME: `old_name`, `old_parent`; TRUNCATE: `old_size`, `new_size`
  (always); SYMLINK: `target`.
- Parser MUST also accept optional `time` (uint64 hrtime), `uid`, `gid`,
  `mode`, `attrs` keys (forward-compatible with an upstream print_event
  extension) and map them into Timestamp/UID/GID when present. When absent,
  leave zero values — NEVER fabricate.
- `records_lost` is the plaintext line after `]`. Parse it tolerantly
  (regex `^(\d+) record\(s\) lost`); zero/absent = lossless.

## Tasks

### Task 1: Command seam (record-and-replay testability)

**Objective:** Wrap every zfs invocation behind an unexported runner var so
tests substitute a fake without the zfs binary.

**Files:**
- Create: `internal/metadata/zfs_events.go`
- Test: `internal/metadata/zfs_events_test.go`

**Step 1: Write failing test**

```go
func TestRunZFSCmdUsesRunner(t *testing.T) {
    orig := zfsRunner
    defer func() { zfsRunner = orig }()
    zfsRunner = func(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
        if args[0] != "events" {
            t.Fatalf("args = %v, want events first", args)
        }
        return []byte("[]"), "", nil
    }
    out, _, err := runZFS(context.Background(), "events", "-j", "tank/data")
    if err != nil || string(out) != "[]" {
        t.Fatalf("runZFS = %q, %v", out, err)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestRunZFSCmdUsesRunner -v`
Expected: FAIL - zfsRunner/runZFS undefined

**Step 3: Write minimal implementation**

```go
// zfsRunner is the seam tests replace. Production wiring execs the zfs CLI:
// argv-only (never a shell string), context timeout, stderr captured.
var zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
    ctx, cancel := context.WithTimeout(ctx, zfsCmdTimeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, "zfs", args...)
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    err := cmd.Run()
    return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

func runZFS(ctx context.Context, args ...string) ([]byte, string, error) {
    return zfsRunner(ctx, args...)
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestRunZFSCmdUsesRunner -v`
Expected: PASS

### Task 2: Strict parser for the pinned wire format

**Objective:** Parse stdout of `zfs events -j` into `[]ObjectEvent` plus
RecordsLost, handling every shape in Contract 3.

**Files:**
- Modify: `internal/metadata/zfs_events.go` (add `parseEventsOutput`)
- Test: `internal/metadata/zfs_events_test.go` (table-driven)

**Step 1: Write failing test**

```go
func TestParseEventsOutput(t *testing.T) {
    tests := []struct {
        name      string
        in        string
        want      []ObjectEvent
        wantLost  uint64
        wantErr   bool
    }{
        {
            name: "empty array no loss",
            in:   "[]\n",
            want: []ObjectEvent{},
        },
        {
            name: "create with name",
            in:   `[{"txg":1234,"object":256,"op":"CREATE","name":"report.pdf"}]` + "\n",
            want: []ObjectEvent{{Op: "create", Key: "report.pdf", Txg: 1234}},
        },
        {
            name: "rename carries old_name",
            in:   `[{"txg":1251,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","old_parent":2}]` + "\n",
            want: []ObjectEvent{{Op: "rename", Key: "new.txt", OldKey: "old.txt", Txg: 1251}},
        },
        {
            name: "truncate sizes",
            in:   `[{"txg":1240,"object":256,"op":"TRUNCATE","name":"f","old_size":1024,"new_size":2048}]` + "\n",
            want: []ObjectEvent{{Op: "truncate", Key: "f", Txg: 1240, SizeOld: 1024, SizeNew: 2048}},
        },
        {
            name: "symlink target",
            in:   `[{"txg":1255,"object":301,"op":"SYMLINK","name":"link","target":"a.txt"}]` + "\n",
            want: []ObjectEvent{{Op: "symlink", Key: "link", Txg: 1255}},
        },
        {
            name: "records lost line after array",
            in:   "[{\"txg\":1,\"object\":2,\"op\":\"REMOVE\"}]\n7 record(s) lost to log wraparound\n",
            want: []ObjectEvent{{Op: "remove", Txg: 1}},
            wantLost: 7,
        },
        {
            name: "future fields tolerated and mapped",
            in:   `[{"txg":9,"object":2,"op":"SETATTR","name":"x","time":500,"uid":1000,"gid":100,"mode":420,"attrs":7}]` + "\n",
            want: []ObjectEvent{{Op: "setattr", Key: "x", Txg: 9, UID: 1000, GID: 100,
                Timestamp: time.Unix(0, 500)}},
        },
        {
            name:    "garbage output",
            in:      "this is not json\n",
            wantErr: true,
        },
        {
            name:    "truncated json",
            in:      `[{"txg":1,`,
            wantErr: true,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            events, lost, err := parseEventsOutput(tt.in)
            if (err != nil) != tt.wantErr {
                t.Fatalf("parseEventsOutput err = %v, wantErr %v", err, tt.wantErr)
            }
            if tt.wantErr {
                return
            }
            if lost != tt.wantLost {
                t.Fatalf("lost = %d, want %d", lost, tt.wantLost)
            }
            if len(events) != len(tt.want) {
                t.Fatalf("got %d events, want %d", len(events), len(tt.want))
            }
            for i := range events {
                if events[i] != tt.want[i] { // ObjectEvent is comparable
                    t.Fatalf("event[%d] = %+v, want %+v", i, events[i], tt.want[i])
                }
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestParseEventsOutput -v`
Expected: FAIL - parseEventsOutput undefined

**Step 3: Write minimal implementation**

```go
// rawEvent mirrors the pinned print_event JSON: all fields optional except
// txg/object/op, plus forward-compatible kernel fields the CLI may emit
// later (time, uid, gid, mode, attrs).
type rawEvent struct {
    Txg       uint64 `json:"txg"`
    Object    uint64 `json:"object"`
    Op        string `json:"op"`
    Name      *string `json:"name"`
    Parent    uint64  `json:"parent"`
    OldName   *string `json:"old_name"`
    OldParent uint64  `json:"old_parent"`
    Target    *string `json:"target"`
    OldSize   int64   `json:"old_size"`
    NewSize   int64   `json:"new_size"`
    TimeNs    uint64  `json:"time"` // hrtime (ns since boot-ish); may be absent
    UID       uint64  `json:"uid"`
    GID       uint64  `json:"gid"`
}

var recordsLostRe = regexp.MustCompile(`(?m)^(\d+) record\(s\) lost`)

// parseEventsOutput parses `zfs events -j` stdout: a JSON array optionally
// followed by a plaintext "N record(s) lost to log wraparound" line.
func parseEventsOutput(out string) ([]ObjectEvent, uint64, error) {
    end := strings.LastIndex(out, "]")
    if end < 0 {
        return nil, 0, fmt.Errorf("metadata: zfs events output has no JSON array: %q", truncateForErr(out))
    }
    var raws []rawEvent
    if err := json.Unmarshal([]byte(out[:end+1]), &raws); err != nil {
        return nil, 0, fmt.Errorf("metadata: parsing zfs events JSON: %w", err)
    }
    var lost uint64
    if m := recordsLostRe.FindStringSubmatch(out); m != nil {
        lost, _ = strconv.ParseUint(m[1], 10, 64)
    }
    events := make([]ObjectEvent, 0, len(raws))
    for _, r := range raws {
        op := strings.ToLower(r.Op)
        e := ObjectEvent{
            Op:  op,
            Txg: r.Txg,
        }
        if r.Name != nil {
            e.Key = *r.Name
        }
        if r.OldName != nil {
            e.OldKey = *r.OldName
        }
        if op == "truncate" {
            e.SizeOld, e.SizeNew = r.OldSize, r.NewSize
        }
        if r.TimeNs != 0 {
            e.Timestamp = time.Unix(0, int64(r.TimeNs))
        }
        e.UID, e.GID = uint32(r.UID), uint32(r.GID)
        events = append(events, e)
    }
    return events, lost, nil
}
```

(Note: `time` is hrtime — nanoseconds from a boot-relative or epoch-relative
clock per platform; treat it as ordering information. Pin the interpretation
comment here; a future upstream change to wall-clock hrtime is an upstream
contract change, caught by the fixture test.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestParseEventsOutput -v`
Expected: PASS

### Task 3: Object-to-key mapping and History filtering

**Objective:** Map events to object keys, honoring `HistoryQuery`
(`MaxEvents`, `Since`) and the provider's History contract:
key-scoped history for non-empty `key` (via the CLI's `-o <object>` filter
when the key's object id is stat-able, else client-side `Key` filtering),
full-dataset stream for empty `key`.

**Files:**
- Modify: `internal/metadata/zfs_events.go`
- Test: `internal/metadata/zfs_events_test.go`

**Step 1: Write failing test**

```go
func TestHistoryFilteringAndMaxEvents(t *testing.T) {
    orig := zfsRunner
    defer func() { zfsRunner = orig }()
    var gotArgs []string
    zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
        gotArgs = args
        return []byte(`[` +
            `{"txg":1,"object":10,"op":"CREATE","name":"a"},` +
            `{"txg":2,"object":11,"op":"CREATE","name":"b"},` +
            `{"txg":3,"object":10,"op":"TRUNCATE","name":"a","old_size":1,"new_size":2}]`), "", nil
    }
    p := &zfsEventsProvider{dataset: "tank/data"}
    events, err := p.History(context.Background(), "/mnt/tank/data", "",
        HistoryQuery{MaxEvents: 2})
    if err != nil {
        t.Fatalf("History: %v", err)
    }
    // Newest-first (ring buffer returns newest first); MaxEvents caps.
    if len(events) != 2 {
        t.Fatalf("got %d events, want 2", len(events))
    }
    // Key filter: only object 10 / key "a" events.
    events, err = p.History(context.Background(), "/mnt/tank/data", "a", HistoryQuery{})
    if err != nil {
        t.Fatalf("History: %v", err)
    }
    if len(events) != 2 || events[0].Key != "a" || events[1].Key != "a" {
        t.Fatalf("key filter: %+v", events)
    }
    _ = gotArgs
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestHistoryFilteringAndMaxEvents -v`
Expected: FAIL - zfsEventsProvider undefined

**Step 3: Write minimal implementation**

```go
type zfsEventsProvider struct{}

func NewZFSEventsProvider() MetadataProvider { return &zfsEventsProvider{} }

func (p *zfsEventsProvider) Name() string { return "zfs-events" }

func (p *zfsEventsProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
    ds, err := ResolveDataset(ctx, bucketPath)
    if err != nil {
        return nil, err
    }
    args := []string{"events", "-j"}
    if q.MaxEvents > 0 {
        args = append(args, "-n", strconv.Itoa(q.MaxEvents*3)) // headroom: key filter happens post-parse
    }
    args = append(args, ds)
    stdout, stderr, err := runZFS(ctx, args...)
    if err != nil {
        return nil, fmt.Errorf("metadata: zfs events %s: %w (stderr: %s)", ds, err, stderr)
    }
    events, lost, err := parseEventsOutput(string(stdout))
    if err != nil {
        return nil, err
    }
    setHistoryDetail(HistoryDetail{Dataset: ds, RecordsLost: lost})

    if q.MaxEvents > 0 && len(events) > q.MaxEvents {
        events = events[:q.MaxEvents]
    }
    if key != "" {
        filtered := events[:0]
        for _, e := range events {
            if e.Key == key || e.OldKey == key {
                filtered = append(filtered, e)
            }
        }
        events = filtered
    }
    if !q.Since.IsZero() {
        filtered := events[:0]
        for _, e := range events {
            if e.Timestamp.After(q.Since) || e.Timestamp.IsZero() {
                filtered = append(filtered, e) // zero timestamps pass: unknown, not old
            }
        }
        events = filtered
    }
    return events, nil
}
```

(Note the `MaxEvents*3` fetch headroom is a heuristic — key filtering is
post-parse because mapping key->object id requires the CLI `-o` inode filter
plus a stat; keep client-side filtering as the correct baseline and add the
`-o` fast path only if a real-host integration test proves the inode
mapping. Document whichever you ship in the leaf report.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestHistoryFilteringAndMaxEvents -v`
Expected: PASS

### Task 4: Probe (detection + dataset + feature flag) and Purge

**Objective:** Probe = DetectZFS short-circuit, ResolveDataset, then
`zfs get -H -o value events <dataset>` == "on" (plus pool feature presence
via the same property check); any failure -> Available=false with Reason,
never an error return unless the caller should hard-fail. Purge =
`zfs events -c <dataset>`.

**Files:**
- Modify: `internal/metadata/zfs_events.go`
- Test: `internal/metadata/zfs_events_test.go`

**Step 1: Write failing test**

```go
func TestProbeDegradesGracefully(t *testing.T) {
    if runtime.GOOS != "darwin" {
        t.Skip("degradation path pinned on hosts without ZFS statfs magic")
    }
    orig := zfsRunner
    defer func() { zfsRunner = orig }()
    zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
        t.Fatalf("zfs must not be exec'd when DetectZFS fails first")
        return nil, "", nil
    }
    p := NewZFSEventsProvider()
    res, err := p.Probe(context.Background(), t.TempDir())
    if err != nil {
        t.Fatalf("Probe returned error: %v (must degrade, not error)", err)
    }
    if res.Available {
        t.Fatalf("Probe = available on non-ZFS fs: %+v", res)
    }
    if res.Reason == "" {
        t.Fatal("Probe must record a Reason when unavailable")
    }
}

func TestProbeAvailableWhenFeatureOn(t *testing.T) {
    if _, err := exec.LookPath("zfs"); err != nil && runtime.GOOS != "darwin" {
        // still runnable via runner seam
    }
    orig := zfsRunner
    defer func() { zfsRunner = orig }()
    // Fake the whole exec surface: DetectZFS is GOOS-bound, so on darwin we
    // drive the dataset+feature logic directly via the unexported probeCore.
    calls := 0
    zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
        calls++
        return []byte("tank/data\n/mnt/tank/data\n\ton\n"), "", nil
    }
    p := &zfsEventsProvider{}
    res, err := p.probeCore(context.Background(), "/mnt/tank/data", true /*assumeZFS*/)
    if err != nil || !res.Available || res.Dataset != "tank/data" {
        t.Fatalf("probeCore = %+v, %v", res, err)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run 'TestProbe' -v`
Expected: FAIL - probeCore undefined

**Step 3: Write minimal implementation**

```go
// probeCore is the GOOS-independent heart of Probe; Probe wraps it with
// DetectZFS. assumeZFS lets tests bypass the statfs check on darwin.
func (p *zfsEventsProvider) probeCore(ctx context.Context, bucketPath string, assumeZFS bool) (ProbeResult, error) {
    ds, err := ResolveDataset(ctx, bucketPath)
    if err != nil {
        return ProbeResult{Available: false, Reason: "dataset resolve: " + err.Error()}, nil
    }
    out, stderr, err := runZFS(ctx, "get", "-H", "-o", "value", "events", ds)
    if err != nil {
        return ProbeResult{Available: false, Reason: "feature check: " + err.Error() + ": " + stderr}, nil
    }
    if strings.TrimSpace(string(out)) != "on" {
        return ProbeResult{Available: false, Reason: "dataset property events is not 'on'", Dataset: ds}, nil
    }
    return ProbeResult{Available: true, Dataset: ds}, nil
}

func (p *zfsEventsProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
    resolved, err := filepath.EvalSymlinks(bucketPath)
    if err != nil {
        return ProbeResult{Available: false, Reason: "path resolve: " + err.Error()}, nil
    }
    isZFS, err := DetectZFS(resolved)
    if err != nil {
        return ProbeResult{Available: false, Reason: "statfs: " + err.Error()}, nil
    }
    if !isZFS {
        return ProbeResult{Available: false, Reason: "filesystem is not ZFS"}, nil
    }
    return p.probeCore(ctx, resolved, true)
}

func (p *zfsEventsProvider) Purge(ctx context.Context, bucketPath string) error {
    ds, err := ResolveDataset(ctx, bucketPath)
    if err != nil {
        return err
    }
    _, stderr, err := runZFS(ctx, "events", "-c", ds)
    if err != nil {
        return fmt.Errorf("metadata: zfs events -c %s: %w (stderr: %s)", ds, err, stderr)
    }
    return nil
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run 'TestProbe' -v`
Expected: PASS (darwin: first test runs, second runs via probeCore; linux
with zfs present may also exercise real paths)

### Task 5: Replay fixture + graceful skip integration test

**Objective:** Commit a fixture of REAL `zfs events -j` output (captured on
a Linux/ZFS host; a hand-built fixture matching Contract 3 byte-for-byte is
acceptable if capture is impossible — mark provenance in a README) and an
integration test that parses it end-to-end, skipping when zfs is absent.

**Files:**
- Create: `internal/metadata/testdata/zfs-events-sample.txt`
- Create: `internal/metadata/testdata/README.md` (provenance + how to regenerate)
- Test: `internal/metadata/zfs_integration_test.go`

**Step 1: Write failing test**

```go
func TestReplayFixtureEndToEnd(t *testing.T) {
    raw, err := os.ReadFile(filepath.Join("testdata", "zfs-events-sample.txt"))
    if err != nil {
        t.Fatalf("fixture missing: %v", err)
    }
    events, lost, err := parseEventsOutput(string(raw))
    if err != nil {
        t.Fatalf("parse: %v", err)
    }
    if len(events) == 0 {
        t.Fatal("fixture parsed to zero events")
    }
    // Every event satisfies the frozen op vocabulary.
    for _, e := range events {
        switch e.Op {
        case "create", "remove", "rename", "link", "symlink", "truncate", "setattr":
        default:
            t.Fatalf("unknown op in fixture: %q", e.Op)
        }
    }
    if lost > 0 {
        t.Logf("fixture reports %d lost records (lossy ring buffer) — expected", lost)
    }
}

func TestRealZFSSkipsWithoutBinary(t *testing.T) {
    if _, err := exec.LookPath("zfs"); err != nil {
        t.Skip("zfs binary not on PATH (macOS dev host) — integration skipped")
    }
    // Real-host path: needs a ZFS dataset with events=on; skip unless
    // ZETAOBJECT_ZFS_TEST_DATASET is set.
    ds := os.Getenv("ZETAOBJECT_ZFS_TEST_DATASET")
    if ds == "" {
        t.Skip("set ZETAOBJECT_ZFS_TEST_DATASET to run real zfs integration")
    }
    // ... exercise Probe + History against the real dataset via the provider.
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestReplayFixture -v`
Expected: FAIL - fixture missing

**Step 3: Write fixture + README**

`testdata/zfs-events-sample.txt` — record-and-replay of
`zfs events -j tank/data` output, exactly matching Contract 3 (array +
optional trailing loss line). If captured on a real host, paste verbatim;
otherwise construct from Contract 3 covering all seven ops, rename with
old_name/old_parent, truncate sizes, symlink target, and one wraparound
loss line. `testdata/README.md` documents the provenance and
regeneration command (`zfs events -j <dataset>` on a host with the
extended-metadata branch).

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -count=1 -v`
Expected: PASS, with `TestRealZFSSkipsWithoutBinary` reporting SKIP on macOS.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing (`go test ./internal/metadata/ -count=1 -v`)
- [ ] Parser matches Contract 3 EXACTLY: uppercase wire ops lowercased, optional keys handled, unknown ops preserved lowercase, records_lost parsed from the plaintext line, future fields (`time`/`uid`/`gid`/`mode`/`attrs`) accepted and mapped
- [ ] No fabricated metadata: absent `time`/`uid`/`gid` leave zero values
- [ ] Every zfs exec: context timeout, argv-only, stderr captured and included in errors
- [ ] Probe degrades (Available=false + Reason, nil error) on every failure mode — never hard-errors for "not available"
- [ ] records_lost is propagated and retrievable (HistoryDetail)
- [ ] Fixture tests pass on a machine with NO zfs binary; real-host test skips with a reason
- [ ] No cgo, no third-party imports, gofmt clean
- [ ] No line-number corruption
- [ ] No scope creep: no endpoint handlers, no parity tests (leaves 03/04)

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale — especially the
MaxEvents headroom heuristic and the key->inode `-o` fast-path decision]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing
- [ ] Parser handles EVERY case in the Task 2 table plus the fixture
- [ ] Wire format matches master Contract 3 (compare against zfs_main.c:8332 print_event)
- [ ] Interface contract satisfied: NewZFSEventsProvider returns a valid MetadataProvider named "zfs-events"; frozen interface NOT extended
- [ ] Code follows conventions (stdlib, errors as values, context timeouts, table-driven)
- [ ] No bugs, no security issues (argv-only exec, no shell interpolation of dataset names — dataset comes from `zfs get` output, never user input; key filtering is in-memory)
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- **The wire format supersedes the original feature brief.** The brief
  claimed lowercase ops and fields `time, mode, attrs, uid, gid` in JSON.
  The branch's actual `print_event` (verified during authoring) emits
  UPPERCASE ops, only `txg/object/op/name/parent` (+ per-op extras), and
  prints records_lost as plaintext AFTER the array. The parser accepts the
  brief's fields as OPTIONAL forward-compatible keys so a small upstream
  print_event extension lands with zero Go changes. If you "simplify" by
  rejecting unknown keys, you will break the fixture test and future
  compatibility — don't.
- **hrtime caveat:** `time` is `gethrtime()` output (zfs_events.c:473) —
  high-resolution, not guaranteed wall-clock. It gives stable ordering.
  Do not present it to users as an absolute wall-clock time without an
  upstream clarification; ObjectEvent.Timestamp zero-value currently means
  "not reported".
- **`-o <object>` filter:** upstream filters by `stat().st_ino` (zfs_main.c
  events handler). Linux inode == ZFS object id; on other platforms this
  may not hold. The safe baseline is client-side Key filtering (Task 3);
  only add the `-o` fast path with a real-host test proving inode==object
  on the target platform.
- **Runner seam discipline:** ALL zfs invocations in this file go through
  `zfsRunner`/`runZFS`. Never call `exec.Command` directly in tests or new
  methods — the replay-fixture testability (and macOS CI) depends on it.
- **Purge is destructive:** `zfs events -c` clears the ring buffer. It must
  stay SigV4-authed at the endpoint layer (leaf 04) and never be called by
  server startup paths.
