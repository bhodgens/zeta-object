# WebDAV Locking - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 4 leaf documents
- **Scope:** WebDAV LOCK/UNLOCK/If-header support (zeta-object#11):
  exclusive write locks with timeout expiry, 423 enforcement on
  PUT/DELETE/MOVE, davfs2-default-config compatibility. ZFS-native
  hardening investigated upstream (zfs-metadata#14: answer likely
  "server-side advisory is the contract"); locks stored in the bucket's
  dotfile metadata area for BOTH bucket kinds, behind a lockStore seam
  for a future kernel-backed store.

## Goal

LOCK on a resource returns a token; conflicting LOCK 423s; writes
without the token 423; timeout expires; davfs2 mounts with defaults.

## Architecture

- `internal/frontend/webdav/lockstore.go`: interface + file-backed
  implementation storing one JSON file per lock under
  `<bucket>/.metadata/.locks/<key>.lock` (token UUIDv4, owner href,
  Depth, Timeout, created). Lazy expiry on access + sweep on interval.
- `internal/frontend/webdav/lockhandler.go`: LOCK/UNLOCK methods,
  `If: (<opaquelocktoken:...>)` header parsing, enforcement hook in the
  write path (PUT/DELETE/MOVE/PROPPATCH check before delegate).
- ZFS note: both bucket kinds use the file-backed store; the kernel
  fence question stays open in zfs-metadata#14 (server-side advisory is
  the documented contract).

## Interface Contracts

### Contract 1: lockStore (FROZEN)

```go
// File: internal/frontend/webdav/lockstore.go
package webdav

type LockInfo struct {
    Token   string    // opaquelocktoken:UUID
    Owner   string    // href from the LOCK body (may be empty)
    Depth   string    // "0" (only depth supported in v1)
    Timeout int64     // seconds remaining at LastRefresh
    Created time.Time
    Key     string    // resource path, slash-led, no trailing slash
}

type lockStore interface {
    Acquire(key string, info LockInfo) (LockInfo, error) // ErrLocked if held
    Refresh(key, token string, timeout int64) (LockInfo, error)
    Release(key, token string) error // wrong token: ErrLockTokenMismatch
    Get(key string) (LockInfo, error) // ErrNotFound when free/expired
    Sweep(now time.Time) int          // remove expired, return count
}
// ErrLocked, ErrLockTokenMismatch, ErrNotFound typed errors.
// newFileLockStore(dir string) file-backed under <dir>/.locks/.
```

### Contract 2: If-header + enforcement (FROZEN)

```go
// File: internal/frontend/webdav/lockif.go
// ParseIfHeader parses RFC 4918 §10.4 If headers carrying lock tokens:
// If: (<opaquelocktoken:uuid>) or lists. Returns the submitted tokens.
func ParseIfHeader(h string) []string

// CheckWriteLock returns nil when the resource may be written: unlocked,
// or locked AND a submitted token matches. Else ErrLocked (-> 423).
func CheckWriteLock(store lockStore, key string, ifHeader string) error
```

## Child Document Index

| # | Document | Type | Dependencies | Est. Context |
|---|----------|------|-------------|-------------|
| 01 | 01-lockstore.md | leaf | none | ~35K |
| 02 | 01-lockif.md (same dir, numbered 02) | leaf | none | ~25K |
| 03 | 03-lockhandler.md | leaf | 01, 02 | ~45K |
| 04 | 04-lock-e2e.md | leaf | 03 | ~35K |

Leaves 01 and 02 are INDEPENDENT (parallel dispatch OK - different
files, disjoint contracts). 03 depends on both; 04 on 03.

## Brief contents per leaf (mandatory: leaf template shape)

- 01: lockstore.go + tests (acquire/conflict, refresh wrong-token,
  release wrong-token, lazy expiry via Get, Sweep count, file
  persistence across store reopen; JSON shape stable; concurrency:
  -race test with 8 goroutines on distinct keys + contention on one
  key).
- 02: lockif.go + tests (single token, token list, tagged lists
  "If: <da:href> (<token>)" tolerated by extracting all
  opaquelocktoken URIs, malformed -> empty list).
- 03: LOCK/UNLOCK HTTP handling (LOCK body XML
  <lockinfo><lockscope/><locktype/><owner>; Depth 0 only - Depth
  infinity -> documented 400; Timeout header "Second-N" honored, capped
  3600; response: 200 + Lock-Token header + prop/lockdiscovery XML
  body; conflicting LOCK -> 423; UNLOCK wrong token -> 409, correct ->
  204) + CheckWriteLock enforcement wired into the write paths
  (PUT/DELETE/MOVE/PROPPATCH before delegation; collection-marker
  writes included) + README webdav Limitations update (locking line ->
  implemented, Depth-infinity + shared locks remain unsupported) +
  docs update for zfs-metadata#14 linkage. Owner-side note: enforcement
  must NOT run for lock management methods themselves.
- 04: e2e `<n+1>-webdav-lock.sh`: LOCK -> conflicting second LOCK 423 ->
  PUT without token 423 -> PUT with If token 201/204 -> UNLOCK 204 ->
  PUT after unlock succeeds -> short-timeout expiry flow. Manual gate
  note: davfs2 default-config mount (README documents the manual step,
  mirroring the owncloud decision.md pattern).

## Dispatch Protocol

Standard (leaf brief + repo context + no-commit; review; commit per
leaf; tracking).

## Coding Conventions

Stdlib-first (UUID: crypto/rand hex - no google/uuid dep); errors
`webdav: `; hyphens in prose; timeouts capped; `.metadata/.locks/` is
NEW server-owned state under the bucket's existing metadata area -
charter check: it is runtime coordination state (like the uploads
staging area), not per-object identity metadata; acceptable. Note this
in 01's brief so the implementing agent does not re-litigate.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-lockstore.md | pending | - | - | |
| 02-lockif.md | pending | - | - | |
| 03-lockhandler.md | pending | - | - | |
| 04-lock-e2e.md | pending | - | - | |

## Integration Test Plan

make test / lint / e2e green (pre-existing failures documented). -race
on the lockstore package tests.

## Review Checklist

- [ ] Contracts verbatim; -race clean.
- [ ] 423 semantics correct (LOCK conflict, write without token).
- [ ] Wrong-token UNLOCK 409, correct 204.
- [ ] README + zfs-metadata#14 linkage present.

## Open Questions

- davfs2 needs LOCK on directories too? (mark: v1 files only; document)
  - resolve during 03 review; if davfs2 blocks, add dir-marker locks as
    a follow-up leaf.

## Test Gate

<!-- Tests-first gate (hierarchical-planning skill v1.3.0, patched 2026-10-24).
     Coverage matrix derived mechanically from leaf task headers; the control
     agent (orchestrator, in-session) refines rows and fills test names at
     Phase 0 dispatch. No implementation leaf dispatches until every row is
     VALIDATED. Delta protocol: lesser of plan/tests updates to the greater.
     Recipe: skill hierarchical-planning -> references/tests-first-gate.md -->

Branch scope: WebDAV LOCK/UNLOCK + If-header enforcement (GH issue #11); master Test Gate says 'N/A — no leaf documents'; e2e case 31 names leaf 04.

| # | Feature (from plan) | Unit test evidence | E2E evidence | Status |
|---|---------------------|--------------------|--------------|--------|
| F1 | lock store (file-backed, expiry, sweep) | internal/frontend/webdav/lockstore_test.go:TestFileLockStore_AcquireAndConflict | scripts/e2e/cases/31-webdav-lock.sh | VALIDATED |
| F2 | LOCK/UNLOCK handlers | internal/frontend/webdav/lockhandler_test.go:TestLOCK_HappyPath; TestUNLOCK_CorrectToken204_WrongToken409 | scripts/e2e/cases/31-webdav-lock.sh | VALIDATED |
| F3 | If-header parsing + write enforcement | internal/frontend/webdav/lockif_test.go:TestParseIfHeader; TestCheckWriteLock; lockhandler_test.go:TestLockEnforcement_PUTWithoutToken423 | scripts/e2e/cases/31-webdav-lock.sh | VALIDATED |
| F4 | batch/lock-bypass parity | internal/frontend/webdav/bughunt_w2_grants_test.go:TestBatchLockBypass_ManifestDeleteOnLockedKeyIs423 | scripts/e2e/cases/31-webdav-lock.sh | VALIDATED |

All 4 rows verified on disk by the survey subagent and spot-checked by
the control agent (2026-10-25). Baseline: `make test` exit 0, coverage 80.5%.

Statuses: PENDING | DELTA | VALIDATED

Delta log: 2026-10-25, all rows, control-agent survey — seeded/placeholder rows
replaced with per-row verified test evidence (4 rows). Tree conforms
to its plan per this matrix.
