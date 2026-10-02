# zeta-object support for ZFS_EV_PRINCIPAL (issue #7) - leaf 11 plan

> **Status:** plan for immediate implementation (this change implements it).

## Meta

- **Parent:** docs/design/zfs-principal-metadata.md section 2c; GH issue #7.
- **Scope:** consume the new optional `principal` field that the
  extended-metadata branch (`69d76c6f0` kernel, `ecb039946` zmetad DB
  layout 8 / wire schema 3) now emits, end to end: version gates, row
  mapping, wire surface, validation.
- **Upstream facts (verified live on zfs-meta, 2026-10-02):**
  - `ZFS_IOC_SET_PRINCIPAL` ioctl + `lzc_set_principal`/
    `lzc_clear_principal` in libzfs_core (registered per thread group;
    tag captured at syscall time only; absence never fabricated).
  - zmetad migrates DB to layout 8 in place (`events.principal INTEGER`,
    NULL = writer did not register).
  - `meta.events_schema_version` moves `2` -> `3` IN LOCKSTEP with the
    layout bump (SCHEMA.md section 1: wire version and layout version
    move together).
  - Live probe (p7probe): registered writer's CREATE/SETATTR/TRUNCATE
    records carry `principal=12648430` (0xC0FFEE); the unregistered
    writer's rows carry NULL. Absence semantics confirmed.

## Contract 1: version gates (internal/metadata/zmetad_db.go)

- `zmetadMaxDBSchemaVersion` 6 -> 8 (layouts 7/8 are additive:
  sync_state.root_id, events.principal - neither renamed anything).
- `zmetadEventsSchemaVersion` becomes a SET: {"2","3"}. Rationale: the
  wire version moves in lockstep with the DB version upstream, so a
  layout-8 DB always says 3 - but the consumer's read path is
  version-agnostic on the wire (it reads named columns, not the wire
  blob), and layout-6 DBs (events_schema_version 2) must keep working.
  A singleton required value would 503 every layout-6 deployment for no
  read-path reason. Min DB version stays 5.

## Contract 2: row mapping (internal/metadata)

- `EventRow` gains `Principal *uint64` (NULL = writer did not register;
  never fabricated, same pointer discipline as UID/GID).
- `Events()` SELECT adds `principal`; scan as NullInt64 -> nullUint64Ptr.
- `ObjectEvent` GAINS a field: `Principal *uint64`. This is the frozen
  contract from metadata-zfs-2026-09 Contract 1 - the pinned rule is
  "add methods/types, never rename/remove"; adding an optional POINTER
  field preserves every existing value shape (zero value = absent, same
  as UID=0 today). All existing constructors keep compiling (field is
  zero-valued when unset).
- `rowsToEvents` maps it: `if r.Principal != nil { e.Principal =
  r.Principal }`. Owners: 01-provider-interface (file), consumers get it
  free.

## Contract 3: wire surface (internal/frontend/s3 capability_endpoints)

- The ?events JSON per-event object gains `"principal": <u64>` with
  `omitempty`-style presence: present ONLY when the pointer is non-nil
  (JSON pointer emission, same pattern the ext XML uses for
  IsLossy/RecordsLost). Layout-6 buckets and unregistered writers
  produce byte-identical output to today (parity preserved).
- ext XML: principal is JSON-surface-only; the ext XML contract does not
  grow (no client asked for it there; additive later if needed).

## Contract 4: gateway integration (leaf 10 bridge, NOT this change's code)

The gateway does NOT register a kernel principal in this change:
registration is per-thread-group, and Go schedules request handlers on
an arbitrary thread pool - a registered tag would attribute every
goroutine on that thread, which is exactly the misattribution issue #7's
revised design warns about. A per-request registration is a kernel
ioctl per request - measurable cost, and the xattr breadcrumbs + audit
log already attribute at request granularity. Deferred: a follow-up
design must either (a) pin each request to a dedicated thread with the
principal registered (runtime.LockOSThread pool keyed by AccessKeyID -
bounded by identity count, not requests), or (b) wait for upstream to
grow a per-IO attribution handle. Documented in the leaf; NOT silently
skipped.

## E2E plan (AGENTS.md hard rule)

- Unit: gate acceptance {5,6,7,8} x {2,3}; refusal {4,9}; EventRow
  mapping with principal NULL/present; JSON presence/absence.
- zfs-validate probe (run-zfs-validation.sh): valuser-registered writer
  (a C probe linking libzfs_core, like the branch's own schema test)
  PUTs through zeta-object? NO - registration is per-process, so the
  probe registers INSIDE a writer process on the host and asserts the
  zmetad row carries the tag; the zeta-object side asserts ?events JSON
  surfaces principal when the column is populated. Both halves in one
  probe step.

## Risks

| Risk | Mitigation |
|---|---|
| Layout-6 hosts refuse after bump | gates accept {2,3} wire x {5..8} DB - matrix-tested |
| Fabricated principal | pointer-only mapping; NULL stays nil end to end |
| Parity gate drift | absent-principal output is byte-identical (omitempty path) |

## Open decisions

1. Thread-pinned per-principal registration (Contract 4) - defer to its
   own leaf; needs a cost measurement first.
