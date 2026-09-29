# MetadataProvider Seam + ZFS Events (Full S3 Metadata Parity) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 4 leaf documents under this node
- **Scope:** An optional, lazily-probed MetadataProvider capability per bucket — first provider reads OpenZFS per-dataset file-event history — surfaced through capability endpoints, gated so it can never drift from canonical S3 metadata behavior.

## Goal

mini-s3 gains an OPTIONAL enrichment capability: a `MetadataProvider` attached to a
bucket, probed lazily (one cheap check per bucket at startup; nil interface when
absent — near-zero overhead when unused). The first concrete provider,
`zfs-events`, reads the user's OpenZFS extended-metadata branch
(`/Users/caimlas/git/zfs-metadata`, branch `extended-metadata`): per-dataset
ring-buffer event logs queried via `zfs events [-j] [-n max] [-c] <dataset>`,
exposing create/remove/rename/link/symlink/truncate/setattr history for objects.

The provider enables capability endpoints (object/bucket event history, a
clearly-marked version-listing EXTENSION derived from events) served under the
existing S3 frontend with the same SigV4 auth and XML/JSON conventions as every
other subresource.

**CRITICAL, NON-NEGOTIABLE REQUIREMENT — FULL S3 METADATA PARITY.** The
canonical metadata surface is owned by tree `object-model-2026-09` leaf 02 and
is exactly: Content-Type, Content-Length, ETag, Last-Modified headers plus
x-amz-meta-* headers. Providers enrich (history, version listing) but NEVER
alter core metadata behavior. Leaf 03 enforces this with a parity gate: the
same request against a plain-FS bucket and a ZFS-backed bucket with the
provider enabled MUST return identical headers and body, and ObjectMetadata
JSON sidecars must remain byte-compatible.

## Architecture

New package `internal/metadata/` holds the frozen `MetadataProvider` interface,
registry, ProbeResult/ObjectEvent types, and the `zfs-events` consumer. The
consumer shells out to `zfs events -j` via `os/exec` (pure Go, static binary
preserved — NO cgo/libzfs_core, NO on-disk `.zfs/events` parsing) with context
timeouts, stderr capture, and a strict JSON parser mapped to the EXACT wire
format of the branch's `print_event` implementation (pinned below — verified
against `cmd/zfs/zfs_main.c:8332`, which differs from the original feature
brief; see Notes). `Probe` uses runtime statfs (ZFS magic per-GOOS with build
tags) plus one `zfs get -H -o value` property check, degrading to a nil
provider on any failure with a logged reason. The fs backend (sibling tree
`backend-interface-2026-09`) calls `Probe` per bucket at startup and attaches
matching providers to `CapabilitySet.MetadataProviders`. Capability endpoints
hang off the existing dispatch in `main.go` as subresource-style query params
(`?events`), reusing SigV4 auth. A parity CI gate (leaf 03) runs identical
request sets against plain-FS and provider-enabled buckets and diffs complete
responses. macOS dev machines have no ZFS: all consumer behavior is testable
with fake providers plus a record-and-replay fixture of real `zfs events -j`
output; integration tests skip gracefully when the `zfs` binary is absent.

## Interface Contracts

### Contract 1: MetadataProvider interface + value types (FROZEN)

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
    Op       string // "create"|"remove"|"rename"|"link"|"symlink"|"truncate"|"setattr"
    Key      string
    OldKey   string
    Timestamp time.Time
    Txg      uint64
    SizeOld, SizeNew int64
    UID, GID uint32
}
```

- **Owner:** 01-provider-interface-probe.md (owns the file and the registry)
- **Consumers:** 02-zfs-events-consumer.md, 03-s3-metadata-parity.md,
  04-capability-endpoints.md; fs Backend from sibling tree
  `backend-interface-2026-09`.
- Leaves refine helper methods but MUST NOT rename/remove these symbols.

### Contract 2: Registry + lazy attach

```go
// File: internal/metadata/registry.go (owner: 01)
package metadata

// Register adds a provider under its Name(). Panics on duplicate names.
func Register(p MetadataProvider)
// Lookup returns the provider registered under name, or nil.
func Lookup(name string) MetadataProvider
// ProbeAndAttach probes every registered provider for bucketPath and returns
// the names of providers whose Probe reports Available. The fs Backend calls
// this once per bucket at startup and copies the result into
// CapabilitySet.MetadataProviders (object-model-2026-09 contract 3).
func ProbeAndAttach(ctx context.Context, bucketPath string) []string
```

### Contract 3: zfs events -j wire format (PINNED from source)

Verified against `/Users/caimlas/git/zfs-metadata/cmd/zfs/zfs_main.c`
`print_event` (line 8332) and `zfs_do_events` (line 8418). This is the EXACT
shape `zfs events -j <dataset>` emits — leaves 02 and 04 must parse THIS, not
the feature brief's aspirational field list:

```
[{"txg":1234,"object":256,"op":"CREATE","name":"report.pdf"},
 {"txg":1240,"object":256,"op":"TRUNCATE","name":"report.pdf","old_size":1024,"new_size":2048},
 {"txg":1251,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","old_parent":2},
 {"txg":1255,"object":301,"op":"SYMLINK","name":"link","target":"a.txt"}]
N record(s) lost to log wraparound        <- OPTIONAL plaintext line AFTER the closing ]
```

Hard facts every leaf must respect:
- Op values are UPPERCASE on the wire ("CREATE", "REMOVE", "RENAME", "LINK",
  "SYMLINK", "TRUNCATE", "SETATTR"); consumers lowercase into `ObjectEvent.Op`.
- Always present: `txg` (num), `object` (num), `op` (str). Optional per-op:
  `name` (str), `parent` (num, omitted when 0); RENAME adds `old_name` (str)
  and `old_parent` (num, omitted when 0); TRUNCATE always adds `old_size`,
  `new_size` (num); SYMLINK adds `target` (str).
- `time` (hrtime), `uid`, `gid`, `mode`, `attrs` exist in the kernel nvlist
  (zfs_events.h:86-99) but the current `print_event` does NOT emit them in
  JSON. The parser MUST accept `"time"`, `"uid"`, `"gid"`, `"mode"`, `"attrs"`
  as optional keys (future-proofing for a small upstream print_event extension)
  and MUST treat zero `Timestamp`/zero `UID, GID` as "not reported by the
  current wire format" — never fabricate values. See leaf 02 Notes.
- `records_lost` arrives as a plaintext human line AFTER the JSON array, not as
  a JSON field. It MUST be surfaced: event history is lossy and any versioning
  UI must know.

### Contract 4: Capability endpoints (surface designed by 04; auth + conventions fixed here)

```
GET /{bucket}/{key}?events            -> object event history, JSON
GET /{bucket}?events                  -> bucket-level event summary, JSON
GET /{bucket}?events&versions         -> version-listing EXTENSION derived
                                         from create/rename/truncate/remove
                                         events (non-standard; response
                                         includes <Lossy>true</Lossy>-style
                                         loss indicator when records_lost>0)
```

- Same SigV4 authentication as every other subresource (single shared
  credential, `sigv4.go` `authenticateRequest`/`authenticatePresigned`).
- Dispatched from `objectLevelDispatch`/`bucketLevelDispatch` in `main.go`
  alongside the existing `?acl`/`?uploads`/`?uploadId` checks, before the
  method switch, exactly like existing subresources.
- XML responses (when used) carry the pinned namespace
  `http://s3.amazonaws.com/doc/2006-03-01/` (`xml.go` `s3XMLNamespace`);
  error responses use `writeS3Error`.

### Contract 5: Parity gate (owner: 03)

The parity test suite builds two buckets — one plain-FS, one with a provider
registered and attached (fake provider suffices; the real zfs-events consumer
is exercised separately via replay fixtures) — replays an identical request
matrix (PUT/GET/HEAD/LIST, with and without x-amz-meta-*, plus multipart
completion) and asserts byte-identical response headers and bodies, ignoring
only timestamps that are legitimately per-object (Last-Modified is compared
within tolerance only if both buckets serve the same fixture file mtime; see
leaf 03). It also asserts ObjectMetadata sidecar JSON in `.metadata/` is
byte-compatible between the two buckets modulo StoragePath.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-provider-interface-probe.md | leaf | none (frozen contract inlined) | 45K | A |
| 02 | 02-zfs-events-consumer.md | leaf | Contract 1 + 3 (frozen, inlined) | 55K | A |
| 03 | 03-s3-metadata-parity.md | leaf | 01 (registry + attach wiring) | 45K | B |
| 04 | 04-capability-endpoints.md | leaf | 01, 02 (History), 03 (parity conventions) | 55K | B |

**Concurrency groups:** A = 01+02 (disjoint files in package `metadata`;
contract text is frozen above so no cross-leaf drift is possible).
B = 03+04. Max 3 per batch.

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group [A]

1. **Read** `01-provider-interface-probe.md` and dispatch via `delegate_task`:
   - Goal: "Implement all tasks from 01-provider-interface-probe.md"
   - Context: Full leaf document text + Contracts 1, 2 from this master +
     Coding Conventions block + current `types.go` (ObjectMetadata) and
     `config.go` (buckets map) excerpts INLINED.
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests, report
     results only."
   - Include: "Do NOT use read_file on existing source files — explore with
     search_files or terminal cat instead. If you read a file, never feed its
     output into write_file."
2. **Read** `02-zfs-events-consumer.md` and dispatch via `delegate_task`:
   - Goal: "Implement all tasks from 02-zfs-events-consumer.md"
   - Context: Full leaf document text + Contracts 1, 3 from this master +
     Coding Conventions block + the replay fixture location.
   - Include the same Do-NOT-commit / no-read_file statements.
3. Wait for both; review per the Review Checklist.

### Phase 2: Dispatch Concurrency Group [B]

1. **Read** `03-s3-metadata-parity.md` and dispatch via `delegate_task`
   (context: leaf text + Contracts 1, 2, 5 + 01's actual exported symbols).
2. **Read** `04-capability-endpoints.md` and dispatch via `delegate_task`
   (context: leaf text + Contracts 3, 4 + 01/02 actual exported symbols +
   `main.go` dispatch excerpts INLINED).
3. Wait for both; review.

### Phase 3: Review and Commit Each Child

After each implementation agent returns, the orchestrator reviews in-session
(the main model reviews directly, NOT a delegated subagent):

1. Read the changed files (from the implementer's file list).
2. Check against leaf spec + interface contracts + Review Checklist below.
3. Run: `go build ./... && go test ./internal/metadata/... ./... -count=1`
   and `gofmt -l .` (must be empty).
4. If gaps: re-dispatch the same leaf with specific feedback (max 3 cycles,
   then escalate). If pass: commit the leaf's exact paths with
   `git add <paths> && git commit -m "feat(metadata): <leaf name>"` and set
   status REVIEWED.

### Phase 4: Integration Review

1. Run the full gate: `make precommit` plus `make test-race`.
2. Cross-boundary checks:
   - Registry round-trip: Register -> ProbeAndAttach -> CapabilitySet names.
   - Parity suite passes with a fake provider attached (leaf 03 target).
   - `?events` endpoints return 503/404-style S3 errors when no provider is
     attached, 200 JSON when attached, and never touch core metadata paths
     (parity re-run must stay green).
   - `go test ./... -run TestZFS -v` on a machine WITHOUT the zfs binary:
     every ZFS-dependent test reports skip, none fail.
   - Cross-compile check: `GOOS=darwin GOARCH=arm64 go build ./...` and
     `GOOS=linux GOARCH=amd64 go build ./...` both succeed.
3. If integration gaps: re-dispatch the owning leaf (no commit), re-review.
4. Normalize formatting (`gofmt`, `goimports -local mini-s3`), verify no
   line-number corruption
   (`grep -rcE '^\s+[0-9]+\|' --include='*.go' .` returns zero), commit any
   integration fixes, set all children COMPLETE.

## Review Checklist

The orchestrator verifies each child in-session:

- [ ] All tasks from the leaf document are implemented
- [ ] Interface contracts from this master are satisfied exactly (symbol names,
      signatures, file paths)
- [ ] All specified files created/modified at exact paths
- [ ] Tests written and passing (TDD followed); ZFS-dependent tests skip
      gracefully without the `zfs` binary
- [ ] Contract 3 wire format parsed EXACTLY (uppercase ops, optional keys,
      plaintext records_lost after `]`)
- [ ] No cgo, no libzfs_core, no on-disk `.zfs/events` parsing
- [ ] Every `os/exec` call carries a context timeout and captures stderr
- [ ] Parity: no leaf changes core Get/Head/Put/List metadata behavior
- [ ] Code follows Coding Conventions; gofmt clean
- [ ] No scope creep, no debug artifacts (no print debugging, TODOs, dead code)
- [ ] No line-number corruption: no `     N|` prefixes in source files

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go 1.25 (module `mini-s3`), stdlib ONLY (os/exec is stdlib;
  no cgo, no third-party deps — go.mod currently has zero requires).
- **Layout:** new package `internal/metadata/`; no imports from `package main`
  (main wires into the package, not vice versa).
- **Naming:** exported = PascalCase with doc comments; unexported = camelCase.
- **Errors as values:** wrap with `%w`; map exec failures to typed errors
  (binary-missing, exit-nonzero, parse) — never panic outside Register.
- **Exec discipline:** every `exec.CommandContext` with timeout, stderr
  captured into the error on failure, input via argv (never shell string).
- **Testing:** table-driven, `_test.go` alongside source, fakes for providers,
  replay fixtures under `internal/metadata/testdata/`; `t.Skip` with reason
  when `zfs` binary or ZFS features are absent.
- **Formatting:** `gofmt` + `goimports -local mini-s3` before reporting.
- **File headers:** none (no copyright boilerplate).

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-provider-interface-probe.md | COMPLETE    | 0 | |
| 02-zfs-events-consumer.md | COMPLETE    | 0 | |
| 03-s3-metadata-parity.md | COMPLETE    | 0 | |
| 04-capability-endpoints.md | COMPLETE    | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. **Unit:** `go test ./internal/metadata/... -count=1 -v` — registry, probe
   matrix (per-GOOS fake statfs), parser against the replay fixture, error
   mapping, parity suite, endpoint handlers — all green.
2. **Parity gate:** `go test ./internal/metadata/ -run TestParity -v` —
   identical response headers+body for FS vs provider-enabled bucket matrix;
   sidecar byte-compat. This is the release blocker for the tree.
3. **Skip-gracefulness:** on macOS (`which zfs` fails), `go test ./... -run ZFS`
   reports every real-consumer test as SKIP, zero FAIL.
4. **Full gate:** `make precommit && make test-race` — build, vet, fmt-check,
   lint (NEW_FROM_REV current rev), tests, race — all green; coverage floor
   (`make test-cover-enforce`, 50%) not regressed.
5. **Cross-compile:** `GOOS=linux GOARCH=amd64 go build ./...` and
   `GOOS=darwin GOARCH=arm64 go build ./...` succeed (probe build tags).
6. **Manual smoke (Linux/ZFS host only):** dataset with `events=on`, create +
   rename + truncate files, `GET /bucket/key?events` returns the JSON history;
   `GET /bucket?events&versions` lists versions with loss indicator;
   aws-sigv4-signed requests only (unsigned → 403).

## Structural Completeness Check (Before Dispatch)

Run after authoring all documents:

```
python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans/metadata-zfs-2026-09 --strict-leaves
```

All sections required by the scan are present in this document and every leaf
carries "Do NOT commit" and a Self-Verification Checklist.

## Notes

- **Dependency on sibling trees:** depends on `object-model-2026-09`
  (canonical metadata types + parity surface, its leaf 02) and
  `backend-interface-2026-09` (Backend seam / CapabilitySet attach point —
  that directory exists but is empty as of authoring; its tree is being
  authored in parallel). This tree is INDEPENDENT of
  `frontend-interface-2026-09` except that leaf 04's endpoint conventions
  must match the existing S3 frontend's conventions (which are already fixed
  in `main.go`/`xml.go`). Because both siblings are in flight, leaves 01-03
  are written to compile standalone against the frozen contracts above; the
  Backend attach call site lands with backend-interface's own tree.
- **WIRE FORMAT SUPERSEDES THE BRIEF:** the feature brief listed record
  fields `time, mode, attrs, uid, gid` and lowercase ops. The branch's actual
  `print_event` (zfs_main.c:8332, verified by grep during authoring) emits
  UPPERCASE ops and only `txg, object, op, name, parent` + per-op extras;
  `records_lost` is a plaintext line after the JSON array. Leaves parse the
  verified format (Contract 3). Recommended upstream follow-up (NOT in scope
  here, file against the zfs-metadata branch): extend `print_event` to emit
  `"time"`, `"uid"`, `"gid"` so ObjectEvent.Timestamp/UID/GID populate; the
  Go parser already accepts those optional keys, so no Go change would be
  needed.
- **Custom bucket paths & symlinks:** buckets may come from the config.json
  `buckets` map (arbitrary paths, symlinks followed) or auto-discovery under
  dataDir. `Probe` receives `bucketPath` after the existing path resolution;
  `filepath.EvalSymlinks` the path before `zfs get`/statfs so dataset
  discovery works for symlinked buckets, and map dataset -> mount via
  `zfs get -H -o value name,mountpoint`.
- **Ring buffer semantics:** history is best-effort and lossy
  (`records_lost`); document in endpoint responses. Purge maps to
  `zfs events -c <dataset>`; keep it explicit and SigV4-authed.
- **Risk areas:** parser strictness vs nvlist omissions (never assume
  optional fields exist); build-tag drift between GOOSes breaking
  cross-compile; parity test flakiness from mtime differences (leaf 03 pins
  the tolerance policy).
