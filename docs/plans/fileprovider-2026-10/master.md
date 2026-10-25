# FileProvider extension client - thin adapter over the zeta-cache daemon

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 3 leaf documents (flat)
- **Scope:** the native macOS client path (zeta-object issue #14): a
  FileProvider extension as a THIN ADAPTER over the zeta-cache daemon -
  the issue's documented Swift/Go boundary option. The daemon (leaves
  01-07 of client-cache-2026-10) owns sync, cache, quota, and the index;
  the extension surfaces the namespace to Finder and drives
  materialization/dehydration through the daemon's IPC + local cache
  files. No signing, no notarization, no App Store - local development
  builds + documented manual enablement (the distribution gate is
  explicit future work).

## User decisions (locked)

- Thin adapter (issue open question, resolved): the Go daemon stays the
  single network-attached, credential-holding process; the extension
  speaks ONLY the local IPC protocol + reads/writes the local cache dir.
- Classic NSFileProviderExtension (the pre-modern decorated API) for
  the source skeleton - it builds without an app target and works with
  a manual pluginkit registration; the modern Replicated extension
  needs an app container (future).
- macOS 13+ target (issue open question, resolved): placeholder
  behaviors dependable from 13+; this Mac runs 26.7.
- Eviction policy stays daemon-owned (leaf 07); the extension only
  reports dehydration to the daemon via IPC.

## Leaves

1. `01-daemon-fileprovider-surface.md` - the daemon-side surface the
   extension needs: IPC additions (enumerate listing with materialized
   state, download-trigger, evict/dehydrate reporting), the local cache
   path exposure rules (the extension reads cache files directly for
   data - same user, same process boundary), LaunchAgent plist artifact,
   README. Gate: unit tests + case 40 IPC asserts extended.
2. `02-extension-source.md` - gui/fileprovider/ Swift sources:
   FileProviderExtension (enumerate via IPC, item identity = server
   fileid, download = IPC download-trigger + local cache file read,
   rename/delete/move = IPC ops over webdav via the daemon),
   FileProviderEnumerator, unit tests for the IPC-to-NSFileProvider
   item mapping. Gate: swift build + swift test green.
3. `03-enablement-docs.md` - the manual enablement path: pluginkit
   registration, per-user launch, signing/notarization requirements
   documented as the distribution gate (explicit future work), README.

## Constraints

- The extension NEVER holds credentials or talks to the network
  (locked leaf-08 property).
- Charter: no server state; enumerate = the daemon's PROPFIND-cached
  index.
- No cgo. Swift only in gui/fileprovider; Go only in internal/.
- Frozen: leaves 01-08 of client-cache-2026-10 (extend IPC additively
  with new request types only; never change existing shapes).

## Tracking

- Parent issue: zeta-object#14.
- Prerequisite: client-cache-2026-10 (DONE).

## Test Gate

<!-- Tests-first gate (hierarchical-planning skill v1.3.0, patched 2026-10-24).
     Coverage matrix derived mechanically from leaf task headers; the control
     agent (orchestrator, in-session) refines rows and fills test names at
     Phase 0 dispatch. No implementation leaf dispatches until every row is
     VALIDATED. Delta protocol: lesser of plan/tests updates to the greater.
     Recipe: skill hierarchical-planning -> references/tests-first-gate.md -->

| # | Feature (from plan) | Source | Unit tests | E2E tests | Status |
|---|---------------------|--------|-----------|-----------|--------|
| F1 | Leaf 01 - daemon FileProvider surface | 01-daemon-fileprovider-surface.md | (pending) | (pending) | PENDING |
| F2 | Leaf 02 - FileProvider extension sources (thin Swift adapter) | 02-extension-source.md | (pending) | (pending) | PENDING |
| F3 | Leaf 03 - enablement + distribution documentation | 03-enablement-docs.md | (pending) | (pending) | PENDING |
| F4 | issue-14-comment.md | issue-14-comment.md | (pending) | (pending) | PENDING |
| F5 | Replicated extension: live-device bringup findings (2026-10-12) | replicated-bringup-findings.md | (pending) | (pending) | PENDING |

Statuses: PENDING | DELTA | VALIDATED

Delta log: (none yet)
