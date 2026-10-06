# zmetad: monotonic event cursor - incremental "events since N" query contract

Status: PROPOSED upstream enhancement. Consumed by the zeta-object gateway's
`?events` surface (wire pass-through) and by the planned zeta-cache sync
client. This issue tracks the upstream half; the zeta-object repo tracks the
consumer half.

## Problem

`zmetad`'s events table already assigns every record a strictly monotonic id
(`id INTEGER PRIMARY KEY AUTOINCREMENT`, insert order = poll order, so also
txg order within a dataset). That id is the natural change-detection cursor
for any incremental consumer: "give me every event with id > N".

Nothing exposes it today. The DB is a poll-time snapshot export; a consumer
that reconnects after being offline must either re-read its entire history
slice or diff by `timestamp`. Timestamps cannot order events safely:

- `timestamp` is `gethrtime()`-derived - monotonic since BOOT, not wall
  clock (SCHEMA.md documents this). It resets on reboot and is not
  comparable across boots.
- `captured_at` is wall-clock at poll time at SECOND resolution. Two
  events in the same poll cycle share the same `captured_at`; the consumer
  cannot tell which of the two it has already seen. Strictly-after
  filtering (`> q.Since`) can silently skip a same-second event.

Both zeta-object surfaces (S3 `?events`, webdav `?events`) currently accept
only `max-events` (a count). The `HistoryQuery.Since` time filter exists in
the Go consumer but is timestamp-based and inherits both defects above.

## What is being asked

A documented, stable query contract for incremental consumption:

1. **Cursor semantics.** A documented guarantee that `events.id` is the
   consumption cursor: monotonic, never reused, stable across purges for
   surviving rows. Consumers store one last-seen id per dataset.
2. **A query shape.** Either (minimum) documentation that consumers may
   issue `SELECT ... WHERE dataset = ? AND id > ? ORDER BY id` directly
   against the DB, or (better) a `--since-id N` option on the zmetad query
   path so the cursor contract lives behind the same pinned consumer
   contract as everything else.
3. **Tie-break + truncation honesty.** When a response is truncated
   (`max-events` reached), the consumer needs the highest returned id so
   the next request resumes exactly. A ring-wrap purge can also remove rows
   the consumer has not seen - the existing `gaps`/loss counters tell it
   data was lost, but the cursor contract should state what the consumer
   should do (fall back to a full rescan, which zeta-cache's design already
   mandates).

## Why upstream (this repo) and not only the consumer

The consumer (zeta-object) can filter by `captured_at` today, but that
loses same-second events and breaks across reboots. The correct ordering
key already exists in the producer's DB; formalizing it is a SCHEMA.md
contract addition (the additive-evolution rule applies: no column changes,
a documented access pattern plus optionally a query flag).

## Consumer contract impact

- zeta-object pins a version RANGE on the zmetad layout (minimum = the
  layout carrying the columns the read path needs). This addition would
  raise that range's minimum only if a new query flag lands; documented
  direct-SQL access needs no version bump.
- `?events` pass-through on the gateway would gain a `since-id` (or
  `cursor`) query parameter mapping onto this contract, served by BOTH the
  S3 and webdav frontends (universality rule).

## Tracking

- Upstream issue: https://github.com/bhodgens/zfs-metadata/issues/17
- Consumer (gateway) issue: https://github.com/bhodgens/zeta-object/issues/15
- In-repo durable copy: this file (issue-zmetad-event-cursor.md).
- Design context: zeta-object `docs/plans/quic-h3-2026-10/` decision 8
  (event cursor is an optimization, never a dependency).
