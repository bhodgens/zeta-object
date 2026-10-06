# Plan (IDEATION - NOT IMPLEMENTED): Crush-Lite Distributed ZFS Storage Backend

> **Status: THEORETICAL / IDEATION.** Nothing in this document is implemented.
> This is a design exploration for cluster-mode zeta-object. Do not treat any
> interface, config key, or file path here as existing code. Tracked by GitHub
> issue (see "Tracking" at the bottom).
>
> Rewritten 2026-10-03 against the current repo shape (post backend-interface,
> frontend-interface, object-model, metadata-zfs, and s3-versioning trees).
> Sections invalidated by the platform's evolution are marked **INVALIDATED**
> and kept only as design-history context. The prior revision's server-type
> matrix (local/nfs/ssh/http) is invalidated; see "Server types" below.

## Context

zeta-object currently uses a local filesystem storage model where objects are
stored directly in `<dataDir>/<bucket>/<object>` behind the pluggable
`backend.Backend` seam (`internal/backend/backend.go`). This plan describes a
"crush-lite" distributed backend: one zeta-object gateway distributes objects
across multiple ZFS storage nodes for redundancy, capacity scaling, and
geographic spread.

**Why "crush-lite"?** Inspired by Ceph's CRUSH (Controlled Replication Under
Scalable Hashing), simplified for zeta-object's scope: deterministic placement
with consistent hashing, no CRUSH maps, no per-object index.

**Charter frame (AGENTS.md).** zeta-object is a dumb gateway; the backing
filesystem is the source of truth; no server-owned persistent state with
identity association (no database, no server-owned index) except the named
exceptions. EVERY mechanism below is shaped by that constraint: the cluster
map is configuration, placement is computed (never stored), and repair walks
the backing filesystems instead of consulting an index.

---

## Decisions (user-locked 2026-10-03)

1. **Cluster map distribution: config.** The cluster map is a config file
   with an epoch number, distributed out-of-band by the operator. No gossip,
   no consensus store (both would be persistent coordinated server state and
   a new failure domain). Nodes reject or fall back on epoch mismatch.
2. **Remote member type: S3 proxy only.** Remote nodes are other zeta-object
   instances reached via a SigV4 S3 client wrapped in a `Backend`
   implementation. No raw NFS members, no SSH/SFTP members.
3. **Divergence policy: last-writer-wins (LWW) with read-repair.** On replica
   mismatch (ETag comparison), the most recent write wins; mismatched
   replicas are repaired on read. No version vectors (server-owned state).
4. **Replication/quorum config: per-bucket**, with global defaults.

## Requirements (user-stated)

- A standalone system must scale (nominally to ~100 nodes) **without
  rebalancing existing data** when nodes are added.
- A fileset (bucket/prefix tree, e.g. `bucket/a/`, `bucket/b/`) may be spread
  across multiple hosts for performance.
- Requests may land on any node; the cluster must service them (routing or
  proxying) without prohibitive extra transfer.
- Arbitrary add/remove/re-add of nodes is seamless. Removing a node makes
  only that node's objects decay - availability loss is scoped to data not
  present elsewhere, never cluster-wide.
- No server-owned object index (charter).

---

## The two-layer design (the load-bearing idea)

Pure crush-lite (one global hash ring) cannot meet the zero-rebalance
requirement: placement computed from `(map, key)` tracks topology, so every
membership change moves ~1/N of objects. A per-object distributed lookup
table can meet it but requires exactly the server-owned per-object state the
charter forbids, plus a new lookup failure domain.

The design that meets all requirements composes both ideas at different
granularities:

### Layer 1 - Assignment table (config, epoch'd, WRITE-ONCE)

```
"assignments": {
  "photos/":      { "nodes": ["a","b","c"], "replication": 2 },
  "photos/2026/": { "nodes": ["d","e"],     "replication": 2 }
}
```

- **Routing is longest-prefix match.** `photos/2026/x.jpg` routes to
  {d,e}; `photos/old.jpg` routes to {a,b,c}.
- **The write-once invariant is what buys zero rebalancing:** a prefix's
  node-set never changes after creation. Adding a node changes only FUTURE
  assignments (new buckets, new prefixes). Every existing object's placement
  stays computable from config alone, forever. No data moves.
- Spreading a hot fileset for performance is explicit: the assignment names
  the node set, and Layer 2 fans objects across it. A hot prefix can be
  split going forward with a finer prefix (e.g. `photos/2026/` -> deeper),
  which affects only new writes.
- Table size is one entry per assigned prefix, not per object - a config
  file every node holds in memory even at 100 nodes. Every node can route
  any key locally; there is no remote lookup and no per-object index.
- Node removal: only assignments naming that node lose replicas. Decay hits
  exactly those objects; repair re-copies within the remaining set members.
- Node re-add: the re-joined node is a repair TARGET only until reconciled
  (LWW by ETag); it never changes what placement computes, only whether a
  replica answers.

**Honest cost:** balancing is operator-managed. The system never auto-
migrates anything; scaling means the operator assigns filesets to nodes as
they join. This is the explicit trade: automatic data movement is given up
in exchange for absolute placement stability. (Precedent: GlusterFS DHT
directory-hash-range layouts, extended in place without migration.)

### Layer 2 - CRUSH-lite within an assignment

For `key` under a prefix assigned to {a,b,c} with replication 2:
`hash(bucket + "/" + key)` walks the (scoped, vnode-weighted) ring for that
assignment's node set, skipping already-selected servers and enforcing
failure-domain spread, until 2 distinct healthy nodes are chosen.

This layer is where the original crush-lite doc survives intact: the hash
ring, virtual nodes, weight-proportional vnodes, failure-domain spread, and
quorum logic all carry over, scoped to the assignment's set instead of the
whole cluster. What dies is the global ring over all servers.

### Cluster state: self-routing, share the map, share nothing else

- **Membership/topology** -> the config-distributed map (decision 1).
- **Liveness** -> NOT in the map. Each node health-probes peers directly
  (cheap stat / S3 HEAD). Liveness is per-node, per-moment routing input,
  never durable state. Nodes may disagree about a peer's health; quorum
  rules absorb it.
- **"Which node serves object X"** -> no such record exists. Any node
  computes the replica set for any key from `(map epoch, key)` and fetches
  from whichever replica responds first.
- **Version skew during transitions**: a node on epoch N and one on N+1
  compute different replica sets for the same key. Mitigations: writes go
  to the UNION of old and new replica sets during a grace window (Dynamo-
  style handoff); reads try both; state is bounded and self-healing once
  epochs converge.

### Node lifecycle semantics

- **Remove:** recompute map without it. Objects whose replica set included
  it fall to N-1 replicas; reads still serve if R-of-remaining is met.
  Availability degrades ONLY for objects that lost replicas until repair
  re-copies them. (This is the "decay only for data not present" rule -
  it falls out of quorum + repair, no special mechanism.)
- **Add:** new assignments target it. Until data arrives, reads miss on the
  new node and fall through to an existing replica. No unavailability
  window. No rebalancing of existing data (Layer 1 invariant).
- **Re-add (stale node):** treated as add - a repair target, never a
  read-preferred source until reconciled against a surviving replica by
  ETag/mtime. Deletes propagate "quorum-ack, best-effort rest" plus the
  repair walker comparing replica SETS.
- **Ambiguous case (named honestly):** an object present on fewer than R
  nodes with no surviving quorum record - LWW cannot distinguish "old copy
  surviving" from "deleted object's last copy". Documented divergence
  window; S3 put-overwrite semantics promise no multi-writer consistency
  anyway. Resolution candidates: treat surviving-copy-wins (safe for data,
  resurrects deletes on the walked set) vs quorum-record-of-delete (needs
  state -> charter problem). OPEN - see Open Questions.
- **Full node loss:** operator removes it from the map; repair restores
  replication factor from surviving replicas. Replicas dropped below R with
  no surviving copy = data gone. That is the durability contract of
  replication factor, stated up front.

### Request lands on node A, serviced by node B

Three cases, in order of preference:

1. **Client routes correctly (the 100-node answer).** Any node can answer
   "who owns prefix P" locally from config. A thin resolver (DNS/LB layer
   or client-side) does the longest-prefix match and the client talks to
   the owning node directly. Zero extra transfer, no re-signing. Proxying
   becomes the rare fallback.
2. **Proxy-stream (default fallback, always works).** A computes the route,
   streams from B, relays to the client. B->A is 1x and A->client is 1x
   (only A's NIC carries both; no disk on A, memory-light). How Ceph RGW
   and other S3 servers handle misrouted requests. Node-to-node replication and
   repair traffic always flows directly between the replica pair - A is
   never a relay for replication, only for misrouted client requests.
3. **307 redirect (opt-in optimization only, NOT default).** A replies
   "ask B". Unreliable in practice: SigV4 signs the Host header, so a
   cross-host redirect needs the client to re-sign; most AWS SDKs only
   handle known region-redirect flows. Many clients fail or hang. Never
   the default path.

---

## Current-repo grounding (what exists, what this plan adds)

Exists and is reused as-is:

- `backend.Backend` seam (`internal/backend/backend.go:33`) - frozen;
  Get/Put/Delete/Stat/List/Buckets/Capabilities, `objectmodel.Error` codes,
  multipart ABOVE the seam.
- Backend registry (`internal/backend/registry.go`) - name -> constructor,
  per-bucket selection via config (`buildBackendLookup`,
  `backend_lookup.go`). The distributed backend is a REGISTRANT (`"crush"`),
  not a rewire of main.go.
- ZFS-native capabilities stay per-node: versioning via `versionStore`
  (snapshots/sidecar/reflink), event history via the `MetadataProvider`
  (zmetad). Neither moves into a distributed interface.

New packages this plan would create (hypothetical paths):

- `internal/backend/crushbackend/` - coordinator + placement (Layer 2).
- `internal/backend/s3proxy/` - SigV4 S3 client implementing
  `backend.Backend` (the remote member type, decision 2).
- Assignment matcher (longest-prefix) + config schema extension
  (`distributed` block: assignments, defaults, epoch).
- Repair walker - filesystem walkers on local roots + List on proxy
  members. No index, no bookkeeping DB. Charter-clean.

---

## INVALIDATED from the 2026-09 revision (kept as design history)

### Server types - INVALIDATED

The original four server types do not fit the platform now that zeta-object
is ZFS-native with its own protocol gateway:

| Old type | Verdict | Why |
|----------|---------|-----|
| `local` | Subsumed | The existing `fs` backend already covers local paths; inside a cluster this is just the local member's backing store. |
| `nfs` | DROPPED (decision 2) | An NFS mount is just `fs` pointed at a mountpoint - already works today with zero new code, and carries no ZFS awareness remotely. Not a distinct type. |
| `ssh` | DROPPED (decision 2) | Running remote `zfs` commands over SFTP fights the architecture: the remote node's own zeta-object gateway already owns its ZFS ops. |
| `http` | KEPT, promoted to the ONLY remote type | Became the `s3proxy` backend (decision 2). |

### Other invalidated elements

- **`StorageBackend` interface sketch (WriteObject/ReadObject/metadata ops/
  optional CreateSnapshot/ListSnapshots).** The frozen `backend.Backend`
  seam exists and differs. ZFS-specific methods became the separate
  `versionStore` and `MetadataProvider` seams - per-node concerns.
- **Global hash ring over ALL servers.** Replaced by Layer 1 assignments +
  Layer 2 rings scoped per assignment.
- **`placementSeed` config key.** Superseded by map epoch.
- **Rebalancing coordinator (`storage/rebalance.go`, RebalancePlan).**
  Replaced by the write-once assignment invariant + repair walker. An eager
  rebalancing coordinator would move data the Layer 1 invariant forbids
  moving, and background movement coordination drifts toward server-owned
  bookkeeping (charter).
- **main.go line-range surgery table.** Stale file/line references; handlers
  now live under `internal/frontend/s3`, backend selection under
  `backend_lookup.go`.
- **Proposed dependencies `pkg/sftp`, `golang.org/x/crypto/ssh`.** Dropped
  with the ssh type. `cespare/xxhash` remains a candidate for Layer 2.

### Surviving from the original revision (still valid)

- Hash-based per-object placement (Layer 2, scoped): deterministic, stable
  mapping, weighted distribution, failure-domain spread.
- Quorum read/write model: parallel writes to N targets, W quorum, rollback
  of failed backends; ordered reads with failover; best-effort delete after
  quorum.
- Failure-handling matrix (1-of-3 down -> write OK read OK; 2-of-3 down ->
  write fails, read serves stale-capable replica; slow backend == failed;
  network partition -> partial writes need repair, reads may be stale).
- Read repair on mismatch (now specified as LWW by ETag, decision 3).
- Chaos-test scenarios: kill backend mid-write, partition, disk-full, slow
  backend (latency injection), concurrent writes to same key.
- Backward compatibility posture: `distributed.enabled` false/absent ->
  current single-node behavior; distributed buckets coexist with local ones;
  existing metadata format unchanged.

---

## Open questions (resolve before any implementation plan tree)

1. **Delete-resurrection ambiguity** (see Node lifecycle): surviving-copy-
   wins vs a quorum delete record. Default lean: surviving-copy-wins +
   documented divergence window; a delete tombstone would be persistent
   server state needing a charter decision.
2. **Epoch convergence UX:** what happens on rejected/fallback requests when
   epochs mismatch - proxy to a current-epoch node? serve-stale-and-warn?
   Operator tooling for map distribution (scp? git? included script?).
3. **Assignment granularity guardrails:** minimum prefix depth, bucket-root
   vs sub-prefix assignments, and whether a bucket may span MULTIPLE
   assignments (implied yes by longest-prefix) - confirm interaction with
   List (a bucket List must fan out over assignment sets and merge, lexically).
4. **Multi-part uploads above the seam:** map upload parts to the same
   assignment/replica set as the final key so Complete assembly is local to
   the replica set.
5. **Health-check cadence and quorum interplay:** how long a peer is marked
   unhealthy before writes shrink the target set, vs failing the write.
6. **e2e + zfs-validate story:** a multi-node e2e harness (two in-process
   or two-subprocess instances on loopback) plus which cases the
   `scripts/zfs-validate` live-host run must gain. Per AGENTS.md, any
   implementation touching the data plane is not done until live-validated.

## Tracking

- GitHub issue: https://github.com/bhodgens/zeta-object/issues/13 (filed
  2026-10-03; single tracking issue for cluster mode / crush-lite
  distribution). Implementation, if ever approved, gets its own plan tree
  under `docs/plans/<tree>/master.md` (flat leaves) referencing this
  document as the ideation record - per AGENTS.md, start trees there, not
  in ad-hoc notes.
