# Gateway: event cursor on the ?events surfaces (since-id pass-through)

Status: **SHIPPED (2026-10-08 — gates green; the parent closes the issue
after review).** What landed: `HistoryQuery.SinceID` (additive struct
field, frozen interface untouched) → `WHERE id > ?`; `?since-id=N` on the
s3 AND webdav `?events` surfaces (400 InvalidArgument on a bad cursor);
per-event additive JSON `id`; plain-dir 503 untouched; e2e case 18
extends with the exact-resume asserts; zeta-cache leaf 05 UNPARKED
(`zeta-cache/internal/sync/cursor.go` + the transport `Events` seam).

## Summary

The `?events` surfaces (S3 `GET /{bucket}?events` and `/{bucket}/{key}?events`,
webdav `GET <collection>?events` and `<file>?events`, h3 by wrapping) accept
only `max-events` today. The zmetad events table has a strictly monotonic
`id` per record - the natural cursor - but no wire surface exposes it. This
issue adds a `since-id` (exact name TBD at design time) query parameter that
passes through to the provider as "events with id > N", on BOTH frontends
(universality rule: ZFS capabilities land in all frontends or not at all).

Upstream anchor: zfs-metadata#17 documents the producer-side cursor contract.
Until that lands, this issue's implementation consumes the existing DB
column through the pinned layout-version range - no upstream change is
required for a first cut; the upstream issue formalizes the contract.

## Why the current shape cannot serve a cursor

- `HistoryQuery.Since time.Time` (internal/metadata/metadata.go) exists and
  the zmetad provider filters on it (zmetad_provider.go), but NO frontend
  ever sets it - the wire only carries `max-events`
  (internal/frontend/s3/capability_endpoints.go maxEventsFromQuery).
- Timestamp-based cursors lose same-second events (strictly-after filter on
  second-resolution `captured_at`) and break across reboots (`timestamp` is
  gethrtime monotonic-since-boot, not wall clock, per upstream SCHEMA.md).
- Truncation: with `max-events` (default 1000, cap 10000) a reconnecting
  client cannot resume where a truncated response stopped without an id to
  resume from.

## Shape (design-time sketch, not a commitment)

- Query: `?events&since-id=N` (plus `max-events` unchanged). Response
  envelope gains nothing initially; the highest returned event id is
  derivable client-side IF the wire form carries it - so the JSON event
  entry likely needs an `id` field (additive; fixtures and parity tests
  updated in the same change).
- Provider: `HistoryQuery` gains a cursor field (new field, not `Since`)
  mapped to `WHERE id > ?` in the zmetad DB layer. The frozen
  MetadataProvider seam MUST NOT gain methods - HistoryQuery is a struct,
  so extending it is seam-legal; verify against internal/metadata/parity
  gate (byte-identical responses with the provider disabled).
- Fallback: plain-dir buckets (no provider) keep the contracted 503
  NotImplemented; the client falls back to PROPFIND ETag scans per
  decision 8. Non-ZFS never breaks.
- Loss honesty: when `recordsLost`/`ringSwaps` advance past the client's
  cursor, the client MUST full-rescan (documented; the envelope already
  carries the counters).

## Acceptance

- e2e cases on both s3 and webdav: write N files, fetch with since-id
  mid-stream, assert exact resume (no duplicate, no gap).
- Parity test: provider-enabled vs plain-FS bucket byte-identical on the
  no-cursor path.
- Live ZFS validation (AGENTS.md hard rule): zfs-meta run with the cursor
  checked against `zfs events -j` ground truth.

## Tracking

- Upstream: https://github.com/bhodgens/zfs-metadata/issues/17
- This issue: https://github.com/bhodgens/zeta-object/issues/15
- In-repo durable copy: docs/plans/issue-gateway-event-cursor.md (this
  file) plus docs/plans/issue-zmetad-event-cursor.md (the upstream issue
  body).
- Client consumer: zeta-cache sync engine (plan tree not yet authored).
