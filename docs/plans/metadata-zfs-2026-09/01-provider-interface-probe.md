# Provider Interface, Registry, Probe - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md (docs/plans/metadata-zfs-2026-09/master.md)
- **Scope:** The `internal/metadata` package skeleton: frozen MetadataProvider interface, value types, name registry, lazy ProbeAndAttach, and the per-GOOS ZFS detection layer.
- **Dependencies:** none — Contract 1/2 from the master are frozen and inlined here.
- **Estimated Context:** 45K (exploration + generation + iteration + overhead)
- **Concurrency Group:** A

## Goal

Create package `internal/metadata/` containing: (1) the frozen
`MetadataProvider` interface and its value types exactly as specified in the
master's Contract 1; (2) a name-keyed registry (`Register`/`Lookup`);
(3) `ProbeAndAttach`, which probes every registered provider for a bucket
path and returns provider names for CapabilitySet attachment; (4) the ZFS
filesystem-detection layer used by Probe: a statfs-based filesystem-type
check with per-GOOS build tags (ZFS magic `0x2f5f2f8b` on Linux,
`fs_type == 'zfs'` via f_mntonname/f_fstypename on FreeBSD, unsupported
report on darwin so macOS dev machines and cross-compiles still build), plus
a `zfs get -H -o value` dataset-resolution helper. Everything is
provider-agnostic; the concrete zfs-events consumer is leaf 02.

## Context

zeta-object is a single-binary Go S3-compatible server, `package main`, stdlib
only. Buckets come from two sources: auto-discovery under config `dataDir`
and the config.json `buckets` map (custom paths, symlinks followed — see
CLAUDE.md Storage Layout). The sibling tree `backend-interface-2026-09` will
own the Backend seam that calls `ProbeAndAttach` per bucket at startup and
copies the returned names into `CapabilitySet.MetadataProviders`
(`object-model-2026-09` master, Contract 3); this leaf only needs to make
that call site possible, not write it.

Key files to understand before implementing:
- `types.go:48` - ObjectMetadata shape (the canonical metadata surface this
  capability must never alter; parity enforced by leaf 03).
- `config.go` - ServerConfig, buckets map, credentials; how bucketPath is
  resolved and validated.
- `go.mod` - module zeta-object, zero third-party requires; keep it that way.
- `/Users/caimlas/git/zfs-metadata/module/zfs/zfs_events.c` - the upstream
  event-log implementation (read-only reference; do not import or link it).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/metadata/metadata.go
package metadata

import (
    "context"
    "time"
)

type MetadataProvider interface {
    Name() string // "zfs-events"
    // Probe: cheap availability check (statfs FS-type + feature check).
    // Called once per bucket at startup.
    Probe(ctx context.Context, bucketPath string) (ProbeResult, error)
    History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error)
    Purge(ctx context.Context, bucketPath string) error
}

type ProbeResult struct {
    Available bool
    Reason    string
    Dataset   string
}

type HistoryQuery struct {
    MaxEvents int
    Since     time.Time
}

type ObjectEvent struct {
    Op        string // "create"|"remove"|"rename"|"link"|"symlink"|"truncate"|"setattr"
    Key       string
    OldKey    string
    Timestamp time.Time
    Txg       uint64
    SizeOld, SizeNew int64
    UID, GID  uint32
}
```

```go
// File: internal/metadata/registry.go
package metadata

// Register adds a provider under its Name(). Panics on duplicate names
// (programmer error, startup-only).
func Register(p MetadataProvider)

// Lookup returns the provider registered under name, or nil.
func Lookup(name string) MetadataProvider

// ProbeAndAttach probes every registered provider for bucketPath and returns
// the names of providers whose Probe reports Available. Errors from
// individual probes are logged (via a package-level Logger hook or discarded
// with the Reason recorded in ProbeResult) and do not abort the loop.
// bucketPath MUST already be resolved through filepath.EvalSymlinks by the
// caller.
func ProbeAndAttach(ctx context.Context, bucketPath string) []string
```

```go
// File: internal/metadata/fsdetect.go (per-GOOS variants)
package metadata

// DetectZFS reports whether the filesystem containing path is ZFS.
// It runs statfs (NOT stat) so it inspects the mounted filesystem, then
// matches the ZFS filesystem magic per GOOS. The boolean result is the
// cheap half of Probe; leaf 02 adds the dataset/feature check.
func DetectZFS(path string) (isZFS bool, err error)
```

Exported names, signatures, and struct fields MUST match byte-for-byte —
leaves 02/03/04 compile against them.

### What This Leaf Consumes

Nothing from siblings. The fs Backend (tree `backend-interface-2026-09`,
authored in parallel) will consume `ProbeAndAttach`; this leaf ships its
side of the seam only.

## Tasks

### Task 1: Package skeleton with frozen types

**Objective:** Create `internal/metadata/metadata.go` with the interface and
value types exactly per Contract 1.

**Files:**
- Create: `internal/metadata/metadata.go`
- Test: `internal/metadata/metadata_test.go`

**Step 1: Write failing test**

```go
package metadata

import (
    "context"
    "testing"
    "time"
)

// fakeProvider is the canonical test double; leaves 02-04 reuse the pattern.
type fakeProvider struct {
    name     string
    probeRes ProbeResult
    probeErr error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
    return f.probeRes, f.probeErr
}
func (f *fakeProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
    return nil, nil
}
func (f *fakeProvider) Purge(ctx context.Context, bucketPath string) error { return nil }

func TestObjectEventZeroValueIsValid(t *testing.T) {
    // The frozen struct must remain assignable/comparable with zero values
    // and its field names/positions must not drift.
    var e ObjectEvent
    if e.Op != "" || e.Txg != 0 || e.SizeOld != 0 || e.SizeNew != 0 ||
        e.UID != 0 || e.GID != 0 || !e.Timestamp.IsZero() {
        t.Fatal("zero-value ObjectEvent drifted from frozen contract")
    }
    q := HistoryQuery{MaxEvents: 10, Since: time.Unix(0, 0)}
    if q.MaxEvents != 10 || q.Since.IsZero() {
        t.Fatal("HistoryQuery drifted")
    }
    var _ MetadataProvider = (*fakeProvider)(nil) // interface shape check
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestObjectEventZeroValueIsValid -v`
Expected: FAIL - no Go files in internal/metadata

**Step 3: Write minimal implementation**

```go
// Package metadata provides optional per-bucket metadata enrichment:
// a MetadataProvider seam probed lazily at startup. Providers enrich
// (event history, version listing) but never alter core S3 metadata
// behavior — see docs/plans/metadata-zfs-2026-09/master.md, Contract 5.
package metadata

import (
    "context"
    "time"
)

// MetadataProvider is the frozen seam contract. Implementations MUST be
// safe for concurrent use: Probe/History/Purge may be called from any
// request goroutine.
type MetadataProvider interface {
    Name() string // "zfs-events"
    // Probe: cheap availability check (statfs FS-type + feature check).
    // Called once per bucket at startup.
    Probe(ctx context.Context, bucketPath string) (ProbeResult, error)
    History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error)
    Purge(ctx context.Context, bucketPath string) error
}

type ProbeResult struct {
    Available bool
    Reason    string
    Dataset   string
}

type HistoryQuery struct {
    MaxEvents int
    Since     time.Time
}

type ObjectEvent struct {
    Op        string // "create"|"remove"|"rename"|"link"|"symlink"|"truncate"|"setattr"
    Key       string
    OldKey    string
    Timestamp time.Time
    Txg       uint64
    SizeOld, SizeNew int64
    UID, GID  uint32
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestObjectEventZeroValueIsValid -v`
Expected: PASS

### Task 2: Registry with duplicate-name panic

**Objective:** Name-keyed registration with Lookup, panic on duplicates.

**Files:**
- Create: `internal/metadata/registry.go`
- Test: `internal/metadata/registry_test.go`

**Step 1: Write failing test**

```go
func TestRegisterAndLookup(t *testing.T) {
    p := &fakeProvider{name: "test-a"}
    Register(p)
    if got := Lookup("test-a"); got != p {
        t.Fatalf("Lookup returned %v, want %v", got, p)
    }
    if got := Lookup("missing"); got != nil {
        t.Fatalf("Lookup of unknown name = %v, want nil", got)
    }
}

func TestRegisterDuplicatePanics(t *testing.T) {
    defer func() {
        if r := recover(); r == nil {
            t.Fatal("duplicate Register did not panic")
        }
    }()
    Register(&fakeProvider{name: "dup-a"})
    Register(&fakeProvider{name: "dup-a"})
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run 'TestRegister' -v`
Expected: FAIL - Register undefined

**Step 3: Write minimal implementation**

```go
package metadata

import "sync"

var (
    regMu sync.RWMutex
    reg   = map[string]MetadataProvider{}
)

// Register adds a provider under its Name(). Panics on duplicate names
// (programmer error; registration happens at startup only).
func Register(p MetadataProvider) {
    regMu.Lock()
    defer regMu.Unlock()
    if existing, ok := reg[p.Name()]; ok {
        panic("metadata: provider already registered: " + p.Name() +
            " (" + nameOf(existing) + ")")
    }
    reg[p.Name()] = p
}

// Lookup returns the provider registered under name, or nil.
func Lookup(name string) MetadataProvider {
    regMu.RLock()
    defer regMu.RUnlock()
    return reg[name]
}
```

(Add the small unexported `nameOf` helper, or inline a simpler panic message
— keep it stdlib-only. Note test isolation: tests that Register must use
unique names or reset the map; prefer unique names per test.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run 'TestRegister' -v`
Expected: PASS

### Task 3: ProbeAndAttach lazy wiring

**Objective:** Probe all registered providers for a bucket path; return
available names; failed/unavailable probes never abort the loop.

**Files:**
- Modify: `internal/metadata/registry.go`
- Test: `internal/metadata/registry_test.go`

**Step 1: Write failing test**

```go
func TestProbeAndAttach(t *testing.T) {
    Register(&fakeProvider{name: "hit", probeRes: ProbeResult{Available: true, Dataset: "tank/data"}})
    Register(&fakeProvider{name: "miss", probeRes: ProbeResult{Available: false, Reason: "not zfs"}})
    Register(&fakeProvider{name: "err", probeErr: context.DeadlineExceeded})

    got := ProbeAndAttach(context.Background(), t.TempDir())
    if len(got) != 1 || got[0] != "hit" {
        t.Fatalf("ProbeAndAttach = %v, want [hit]", got)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestProbeAndAttach -v`
Expected: FAIL - ProbeAndAttach undefined

**Step 3: Write minimal implementation**

```go
// ProbeAndAttach probes every registered provider for bucketPath and returns
// the names of providers whose Probe reports Available. Probe errors are
// treated as "not available" (the provider's Reason or the error text is
// preserved for logging by callers via ProbeResult fields where possible)
// and never abort the loop. bucketPath must be symlink-resolved by the
// caller.
func ProbeAndAttach(ctx context.Context, bucketPath string) []string {
    regMu.RLock()
    providers := make([]MetadataProvider, 0, len(reg))
    for _, p := range reg {
        providers = append(providers, p)
    }
    regMu.RUnlock()

    var attached []string
    for _, p := range providers {
        res, err := p.Probe(ctx, bucketPath)
        if err != nil {
            continue // probe failure = not available; logged by caller hook
        }
        if res.Available {
            attached = append(attached, p.Name())
        }
    }
    return attached
}
```

(Deterministic order note: map iteration is random. Either sort the result
with `sort.Strings` in tests/implementation, or accept set semantics — pin
one choice and document it. Prefer sorting for stable startup logs.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestProbeAndAttach -v`
Expected: PASS

### Task 4: Per-GOOS ZFS filesystem detection (statfs, build tags)

**Objective:** `DetectZFS(path) (bool, error)` via statfs with per-GOOS
magic/type matching; darwin reports "unsupported, not ZFS" so macOS dev
machines and cross-compiles build clean.

**Files:**
- Create: `internal/metadata/fsdetect_linux.go` (build tag `//go:build linux`)
- Create: `internal/metadata/fsdetect_bsd.go` (build tag `//go:build freebsd || netbsd || dragonfly`)
- Create: `internal/metadata/fsdetect_other.go` (build tag `//go:build !linux && !freebsd && !netbsd && !dragonfly`)
- Test: `internal/metadata/fsdetect_test.go`

**Step 1: Write failing test** (portable part; runs on any GOOS)

```go
func TestDetectZFSOnNonZFSPath(t *testing.T) {
    // tmpfs/apfs/ext4/whatever the test host uses: never ZFS.
    isZFS, err := DetectZFS(t.TempDir())
    if err != nil {
        t.Fatalf("DetectZFS: %v", err)
    }
    if isZFS {
        t.Fatal("DetectZFS reported ZFS for temp dir")
    }
}

func TestDetectZFSMissingPath(t *testing.T) {
    if _, err := DetectZFS(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
        t.Fatal("expected error for missing path")
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestDetectZFS -v`
Expected: FAIL - DetectZFS undefined

**Step 3: Write minimal implementation**

Linux variant:

```go
//go:build linux

package metadata

import (
    "syscall"
)

// zfsSuperMagic is ZFS_SUPER_MAGIC from linux/magic.h.
const zfsSuperMagic = 0x2f5f2f8b

// DetectZFS reports whether the filesystem containing path is ZFS.
func DetectZFS(path string) (bool, error) {
    var st syscall.Statfs_t
    if err := syscall.Statfs(path, &st); err != nil {
        return false, &PathStatError{Path: path, Err: err}
    }
    return st.Type == zfsSuperMagic, nil
}
```

BSD variant (FreeBSD et al. — f_fstypename string match):

```go
//go:build freebsd || netbsd || dragonfly

package metadata

import "syscall"

// DetectZFS reports whether the filesystem containing path is ZFS.
// BSD statfs exposes the filesystem type as the f_fstypename string;
// ZFS mounts report "zfs".
func DetectZFS(path string) (bool, error) {
    var st syscall.Statfs_t
    if err := syscall.Statfs(path, &st); err != nil {
        return false, &PathStatError{Path: path, Err: err}
    }
    return fstypename(&st) == "zfs", nil
}

func fstypename(st *syscall.Statfs_t) string {
    b := st.Fstypename[:]
    for i, c := range b {
        if c == 0 {
            return string(b[:i])
        }
    }
    return string(b)
}
```

Fallback variant (darwin and any other GOOS):

```go
//go:build !linux && !freebsd && !netbsd && !dragonfly

package metadata

// DetectZFS reports false on platforms without a usable statfs ZFS signal.
// macOS dev machines have no ZFS; this keeps `go build` and cross-compiles
// green. Reason strings for Probe come from the caller.
func DetectZFS(path string) (bool, error) {
    return false, nil
}
```

Shared error type in `metadata.go` (or a new `errors.go`):

```go
// PathStatError reports a failed statfs during ZFS detection.
type PathStatError struct {
    Path string
    Err  error
}

func (e *PathStatError) Error() string {
    return "metadata: statfs " + e.Path + ": " + e.Err.Error()
}
func (e *PathStatError) Unwrap() error { return e.Err }
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestDetectZFS -v`
Expected: PASS on any host

Also verify cross-compile: `GOOS=darwin GOARCH=arm64 go build ./...` and
`GOOS=linux GOARCH=amd64 go build ./...` — both succeed.

### Task 5: Dataset resolution helper (zfs get -H -o value)

**Objective:** Map a symlink-resolved bucketPath to a ZFS dataset name via
`zfs get -H -o value name,mountpoint`, so probe/history can speak to
`zfs events <dataset>`. Must degrade cleanly when the zfs binary is absent.

**Files:**
- Create: `internal/metadata/zfs_cmd.go`
- Test: `internal/metadata/zfs_cmd_test.go`

**Step 1: Write failing test**

```go
func TestResolveDatasetNoZFSBinary(t *testing.T) {
    if _, err := exec.LookPath("zfs"); err == nil {
        t.Skip("zfs binary present; negative-path test not applicable")
    }
    _, err := ResolveDataset(context.Background(), t.TempDir())
    var missing *ZFSBinaryMissingError
    if !errors.As(err, &missing) {
        t.Fatalf("ResolveDataset err = %v, want ZFSBinaryMissingError", err)
    }
}

func TestResolveDatasetEmptyPath(t *testing.T) {
    _, err := ResolveDataset(context.Background(), "")
    if err == nil {
        t.Fatal("expected error for empty path")
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/metadata/ -run TestResolveDataset -v`
Expected: FAIL - ResolveDataset undefined

**Step 3: Write minimal implementation**

```go
package metadata

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "os/exec"
    "strings"
    "time"
)

// zfsCmdTimeout bounds every zfs invocation. Probes run at startup once per
// bucket; history reads are per-request — both must never hang the server.
const zfsCmdTimeout = 5 * time.Second

// ZFSBinaryMissingError means the zfs CLI is not on PATH.
type ZFSBinaryMissingError struct{ Err error }

func (e *ZFSBinaryMissingError) Error() string {
    return "metadata: zfs binary not found on PATH"
}
func (e *ZFSBinaryMissingError) Unwrap() error { return e.Err }

// ResolveDataset returns the ZFS dataset name mounting path, or an error.
// It shells out to `zfs get -H -o value name,mountpoint <path>` — pure Go
// (os/exec is stdlib), no cgo, preserving the static binary. path must be
// symlink-resolved (filepath.EvalSymlinks) by the caller.
func ResolveDataset(ctx context.Context, path string) (string, error) {
    if path == "" {
        return "", errors.New("metadata: empty bucket path")
    }
    ctx, cancel := context.WithTimeout(ctx, zfsCmdTimeout)
    defer cancel()

    cmd := exec.CommandContext(ctx, "zfs", "get", "-H", "-o", "value", "name,mountpoint", path)
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    if err := cmd.Run(); err != nil {
        if _, lookErr := exec.LookPath("zfs"); lookErr != nil {
            return "", &ZFSBinaryMissingError{Err: lookErr}
        }
        return "", fmt.Errorf("metadata: zfs get name,mountpoint %s: %w (stderr: %s)",
            path, err, strings.TrimSpace(stderr.String()))
    }
    lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
    if len(lines) != 2 {
        return "", fmt.Errorf("metadata: unexpected zfs get output for %s: %q", path, stdout.String())
    }
    name, mountpoint := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
    // The dataset's mountpoint must actually be the bucket path (or an
    // ancestor anchor); zfs get on a path inside the dataset resolves it.
    if mountpoint == "" || name == "" || name == "-" {
        return "", fmt.Errorf("metadata: path %s is not in a ZFS dataset", path)
    }
    return name, nil
}
```

(Verify the exact `zfs get -o` multi-property output shape on a Linux/ZFS
host before merging if available; the two-line `-H -o value` split above is
the pinned expectation — see master Notes. If the host's zfs prints a
headerless single line per property in a different order, parse by matching
mountpoint == path or strings.HasPrefix(path, mountpoint+"/") and take name
from the other line; leaf 02's integration test on a real host validates this.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/metadata/ -run TestResolveDataset -v`
Expected: PASS (macOS: the first test skips its zfs-present branch and the
binary-missing path returns the typed error)

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing (`go test ./internal/metadata/ -count=1 -v`)
- [ ] Interface contracts (above) satisfied exactly — symbol names, field names, signatures byte-for-byte vs master Contract 1/2
- [ ] All files at exact specified paths; build tags present on per-GOOS files
- [ ] `GOOS=darwin GOARCH=arm64 go build ./...` and `GOOS=linux GOARCH=amd64 go build ./...` succeed
- [ ] No cgo, no third-party imports (go.mod still has zero requires)
- [ ] gofmt clean (`gofmt -l internal/metadata/` empty)
- [ ] No deviations from spec (or deviations documented below)
- [ ] No scope creep — only what the tasks specify (no History implementation, no JSON parsing — those are leaf 02)
- [ ] No line-number corruption (no `N|` prefixes in any file)

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing
- [ ] Interface contracts match exactly (signatures, types, file paths, build tags)
- [ ] Code follows project conventions (stdlib only, errors as values, table-driven tests, context timeouts)
- [ ] ProbeAndAttach never aborts on a single provider failure and returns deterministic (sorted) names
- [ ] No bugs, no security issues (argv-only exec, no shell string, stderr captured)
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The registry is process-global by design (single binary, startup-only
  registration). Tests must use unique provider names to avoid cross-test
  panics from the duplicate check.
- `DetectZFS` is intentionally separate from dataset resolution: Probe's
  cheap half (statfs) short-circuits before any exec on non-ZFS
  filesystems, keeping per-bucket startup cost at one syscall for the
  overwhelmingly common non-ZFS case.
- FreeBSD's `Statfs_t` exposes `Fstypename` as a fixed-size byte array;
  netbsd/dragonfly may differ slightly — if the build fails on a GOOS you
  cannot test, narrow the BSD tag to `freebsd` only and leave others on the
  fallback path. Do NOT guess field names.
- The frozen `ObjectEvent.Timestamp time.Time` will stay zero under the
  current upstream wire format (no `time` field emitted) — see master
  Contract 3. Do not "fix" this by fabricating timestamps; leaf 02 documents
  the upstream follow-up.
