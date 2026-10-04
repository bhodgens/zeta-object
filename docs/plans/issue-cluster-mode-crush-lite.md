## Summary

Cluster-mode / crush-lite distribution for zeta-object: spread buckets and
prefix filesets across multiple ZFS storage nodes with deterministic
placement, quorum reads/writes, and zero rebalancing of existing data when
nodes join. This issue TRACKS the ideation only - no implementation is
planned or approved yet.

Full design exploration lives in `docs/plan-distributed-zfs-backing.md`
(rewritten 2026-10-03 as an IDEATION document; theoretical, nothing
implemented).

## Decisions locked during ideation (user, 2026-10-03)

1. Cluster map = config (epoch'd, operator-distributed). No gossip/consensus.
2. Remote member type: S3 proxy only (new `s3proxy` Backend implementation).
   No NFS/SSH member types.
3. Divergence policy: last-writer-wins by ETag with read-repair.
4. Replication/quorum config: per-bucket with global defaults.

## Core design (see the plan doc for full detail)

- **Two-layer placement:** (1) write-once, epoch'd, config-distributed
  assignment table mapping bucket/prefix -> node set (longest-prefix match);
  (2) crush-lite consistent hashing within each assignment's node set
  (vnodes, weights, failure-domain spread). The write-once invariant gives
  zero-rebalance node adds; the per-assignment ring gives fileset-level
  performance spread.
- **Self-routing:** every node computes any key's replica set from
  (map epoch, key) - no per-object lookup table, no server-owned index
  (charter-clean). Liveness is per-node health probing, never durable state.
- **Node lifecycle:** remove = decay scoped to that node's objects (quorum
  + repair); add = future assignments only, no data movement; re-add =
  repair target until ETag-reconciled.
- **Misrouted requests:** client-side prefix routing (preferred),
  proxy-streaming fallback; 307 redirects ruled out as default (SigV4 signs
  Host; most SDKs cannot follow cross-node redirects).

## Charter notes

- Cluster map is config, not server state. Placement is computed, never
  stored. Repair walks backing filesystems / Lists proxy members - no
  index, no bookkeeping DB. If any implementation later wants a delete
  tombstone or quorum-record store, that is a NEW charter decision
  (AGENTS.md) and stops here first.

## Known open questions (block any future implementation tree)

- Delete-resurrection ambiguity under LWW (surviving-copy-wins vs quorum
  delete record).
- Epoch-mismatch request handling and operator tooling for map distribution.
- List semantics across multiple assignments in one bucket (fan-out + merge).
- Multipart upload part routing (same replica set as final key).
- Health-check cadence vs quorum shrink behavior.
- Multi-node e2e harness + scripts/zfs-validate live-host coverage design.

## Acceptance criteria (for the ideation phase - all met)

- [x] Plan doc rewritten against current repo state (post backend-interface,
      object-model, frontend split); invalidated sections marked as such.
- [x] Zero-rebalance scaling approach chosen and justified (write-once
      assignments) vs per-object lookup table (rejected: charter + failure
      domain).
- [x] Routing/transfer behavior for misrouted requests specified.
- [x] Node add/remove/re-add semantics specified.
- [x] Open questions enumerated.

## Dependencies

- Builds on (landed): backend-interface-2026-09 (Backend seam + registry),
  object-model-2026-09, frontend-interface-2026-09.
- Implementation, if approved, starts a new plan tree
  `docs/plans/<tree>/master.md` per AGENTS.md; this issue is the pointer.
