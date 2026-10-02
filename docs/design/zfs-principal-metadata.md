# Design: Storing *Which Principal* in ZFS Metadata — Auth/Audit Substrate

Status: DESIGN ONLY — research and recommendation, no implementation. Written
2026-10-01 in response to the question *"is it possible to store which
principal has access to the data in the zfs metadata?"*

Terminology, defined at first use (plain language):

- **ZFS user property** — a key/value annotation on a *dataset* (a whole ZFS
  filesystem), set with `zfs set name:value=... <dataset>`. Dataset-granular;
  values are arbitrary strings up to 8192 bytes; they are inherited by child
  datasets and have no effect on ZFS behavior.
- **xattr (extended attribute)** — a name/value pair attached to a *single
  file*. ZFS stores xattrs either in a hidden per-file directory
  (`xattr=dir`) or inline in the file's metadata (`xattr=sa`). The Linux
  `user.*` namespace is the general-purpose one applications may use.
- **txg (transaction group)** — the unit of ZFS write commit. All changes
  accepted between two commit points share one txg number; it orders and
  identifies groups of mutations.
- **principal** — the external identity zeta-object authenticates: an
  `auth.Identity` (AccessKeyID + bucket grants). The gateway runs as a single
  POSIX uid, so from the kernel's point of view every request comes from the
  same process identity.
- **event log** — the extended-metadata branch's per-dataset ring buffer of
  file-level change records (`zfs events -j`), exported to SQLite by
  `zmetad` (contract: `contrib/zmetad/SCHEMA.md`, layout v6).

## 1. Direct answer

**Yes, ZFS can store principal-related metadata — but no ZFS mechanism stores
a *zeta-object principal* natively, and the event log records only the acting
*POSIX uid* (a number local to the host), not an identity from your auth
model.** Per mechanism:

### (a) ZFS user properties — dataset granularity only

`zfs set zeta:owner=<principal> tank/bucket` works and persists in the
dataset ([zfsprops.7, "User
Properties"](https://openzfs.github.io/openzfs-docs/man/master/7/zfsprops.7.html):
"user properties have no effect on ZFS behavior, but applications or
administrators can use them to annotate datasets"; values are arbitrary
strings, ≤ 8192 bytes, always inherited). But the unit of storage is the
dataset. There is **no per-object user property**. Useful for bucket-level
owner/ACL annotations; useless for "which principal touched object Z."

### (b) File xattrs — per-object, survives restarts, charter-compatible

`setfattr -n user.zeta.principal -v <access-key-id> <object-path>` attaches
a principal stamp to the object file itself ([zfsprops.7,
`xattr`](https://openzfs.github.io/openzfs-docs/man/v2.4/7/zfsprops.7.html):
`xattr=on|off|dir|sa`, dir-style "imposes no practical limit on either the
size or number of attributes"; sa-style stores up to 64K inline). Key
properties:

- **Lives in the filesystem, on the object.** It rides snapshots, `zfs
  send/recv`, and ordinary file copies (dir-style), and survives gateway
  restarts by construction.
- **Not server-owned persistent state** in the charter's sense — the charter
  forbids a store that "outlives the objects it describes." An xattr
  *describes the object itself*, exactly like its mode bits. It is the same
  category as the per-object `.meta` sidecar, but attached by the filesystem
  rather than a sibling file.
- Caveats: the `user.*` namespace is only writable by the file's owner (or
  root/CAP_FOWNER) — fine here, the gateway owns the files it writes. An
  xattr write mutates the file (new txg, new event-log record), so stamping
  doubles write-path syscalls unless batched with the data write. `xattr=sa`
  datasets on other platforms cannot read sa-style xattrs
  ([zfsprops.7](https://openzfs.github.io/openzfs-docs/man/v2.4/7/zfsprops.7.html));
  use `xattr=dir` semantics or accept the portability note.

### (c) Event log record shape — carries uid/gid of the *acting process*

Per the pinned consumer contract (zfs-metadata `contrib/zmetad/SCHEMA.md`,
layout v6, `events` table), the record always carries `txg`, `timestamp`,
`object_id`, `event_type`; op-constrained columns add path/parent and sizes.
The **`uid`/`gid` columns exist and are populated whenever the wire record
carries them — in practice on CREATE and WRITE/READ rows** (also from the
kernel side: `module/zfs/zfs_events.c` fills the record from
`crgetuid(cred)` — the credential of the *process doing the syscall*). Two
consequences:

1. **The uid is the gateway's own single POSIX uid** on every record the
   gateway causes. It identifies the acting process, not a zeta-object
   principal. Principal identity is *structurally lost* at this layer — the
   kernel never learns about SigV4 access keys.
2. The event log is a **fixed-shape record** at the wire level: there is no
   free-form "comment"/principal field to stuff an access-key-id into.
   Extending the wire would mean extending the upstream branch (a real
   option — it is user-owned — but an upstream change, not a gateway-side
   one).

Note also: TRUNCATE/SETATTR/WRITE/READ rows carry **no path/name** — the
consumer reconstructs names from CREATE/RENAME history (SCHEMA.md §7, the
`objmap`/`full_path` mechanism). Any principal-attribution scheme must
survive that same path-reconstruction indirection.

### (d) NFSv4 ACLs / aclmode — real per-object access control, wrong tool for audit

ZFS supports NFSv4-style ACLs per file (`acltype=nfsv4`) and POSIX ACLs as
xattrs (`acltype=posix`) ([OpenZFS docs, "ACLs and Extended
Attributes"](https://openzfs.github.io/openzfs-docs/Basic%20Concepts/Datasets/ACLs.html);
[zfsprops.7, `aclmode`/`aclinherit`](https://openzfs.github.io/openzfs-docs/man/v2.3/7/zfsprops.7.html)).
Two problems for this use case:

- **On Linux, `acltype=nfsv4` is not supported** (zfsprops.7: "The nfsv4 ZFS
  ACL type is not yet supported on Linux") — and zeta-object's validation
  host is Ubuntu/Linux. POSIX ACLs work but encode *user/group IDs with mode
  semantics*, not external principals.
- **`aclmode`/`aclinherit` are destructive by default**: `aclmode=discard`
  "deletes every ACE except those representing the requested mode" on
  `chmod`; defaults "favour Unix mode semantics." zeta-object's own atomic
  write path (temp file + rename + mode fixups) would fight the ACL layer
  constantly. ACLs answer "*who may access*," not "*who did access*" — an
  enforcement primitive, not an audit record. Keep ACLs out of scope here;
  they are a candidate enforcement layer *if* per-uid file access is ever
  introduced, which this design does not propose.

### (e) What the extended-metadata branch adds

The `events`/`events_size` dataset properties and the ring buffer plus
`zmetad` export are the substrate (a). Also relevant from SCHEMA.md v6:

- WRITE/READ ops exist only when `events_io` is enabled; io windows coalesce
  multiple writers, and per upstream `zfs_events.c`, "merged windows may span
  multiple callers; uid/gid are those of the [window's] first" — i.e. even
  the acting-uid attribution is approximate under coalescing.
- `gaps`/`records_lost`: history is **lossy** (ring wraparound). Any audit
  story built on the event log must surface loss, as `?events` already does.
- `captured_at` gives ingest wall-time; the kernel `timestamp` is monotonic
  since boot, not wall clock.

### Summary table

| Mechanism | Per-object? | Principal-aware? | Durable? | Charter OK? |
|---|---|---|---|---|
| User property (`zfs set zeta:…`) | no (dataset) | free string | yes | yes |
| `user.*` xattr | yes | free string | yes (rides object) | yes |
| Event log `uid`/`gid` | yes | **no** (acting POSIX uid = gateway) | ring buffer, lossy | yes (existing) |
| NFSv4 ACL | yes | POSIX ids only | yes | enforcement-only, not audit; Linux nfsv4 unsupported |

## 2. Recommended model

The tension: ZFS events record the acting POSIX uid; the gateway is one uid;
therefore the event log alone can never answer "principal X did Y to Z."
Options considered:

| Option | Verdict |
|---|---|
| **Per-request setuid** (run file I/O as a uid derived from the principal) | Rejected. Requires root, a uid per principal, per-request credential switching in a concurrent server (setuid is process-wide in Go), and reimplements half of POSIX access control in the gateway. Also contradicts "dumb gateway, one process." |
| **Dataset user properties** (`zeta:owner`, `zeta:grants`) | Partial. Right shape for *bucket-level* policy annotations readable by anything on the host (`zfs get`), wrong granularity for object-level audit. Keep as a complementary, not primary, mechanism. |
| **txg correlation**: gateway writes an in-memory record of (txg ↔ principal) at request completion, then joins it to event-log rows by txg | Rejected as *primary*: requires the gateway to learn the txg of its own writes (not exposed by the POSIX API without libzfs), and the correlation table is server-owned state with identity association — charter-forbidden if persisted, and useless if not (restart = correlation loss). |
| **Gateway-side audit trail only** (JSON-lines log of principal/op/key/time) | Necessary but not sufficient (below). As the *sole* mechanism it puts the durable audit record in a server-owned file — the exact thing the charter forbids — unless the file lives *in the bucket* as an object, which is circular (the audit log's own writes need attributing). |
| **Xattr principal stamp on write** (recommended) | See below. |

### Recommendation: xattr principal-stamp, event log as the timeline, joined by object identity

One sentence: **on every mutating operation, the gateway stamps the
authenticated principal into a `user.zeta.*` xattr on the object file; the
ZFS event log remains the *timeline* of mutations; the xattr is the
*attribution*, read back and joined at serve time.**

Concretely, the durable record for "principal X did operation Y to object Z
at time T" decomposes across two ZFS-native stores:

1. **Attribution (xattr, authoritative):** on PUT / multipart-complete /
   COPY / DELETE-then-recreate, the backend sets, in the same code path that
   already writes the object (and alongside the existing `.meta` write):
   - `user.zeta.owner` = AccessKeyID of the authenticated principal (stable
     identity of the current `Identity` model)
   - `user.zeta.last_write_op` = operation (`put` | `multipart` | `copy`)
   This is one `fsetxattr` per write, on a file the gateway already owns.
   It is not "server-owned state": it is object metadata, exactly like the
   ETag already in the `.meta` sidecar — it dies with the object, rides
   snapshots and replication, and needs no schema outside the filesystem.
2. **Timeline (event log, existing):** `?events` continues to serve the
   mutation history (txg, op, size) from zmetad. The event log's uid column
   is **documented as always-equal-to-the-gateway-uid** — it stays in the
   surface (never fabricated) but is not treated as principal identity.
3. **The join:** for object Z, the audit answer is
   `History(Z) × xattrs(Z)`: the event log says *what happened when*; the
   xattr says *who is currently accountable*. This is deliberately
   "current-owner attribution, full timeline" — see tradeoffs.

Tradeoffs, honestly stated:

- **What you lose vs. per-event principal attribution:** the xattr gives the
  *last* writer, not who did each historical mutation. A complete
  principal-per-event history would require either the upstream branch to
  grow an opaque principal field in event records (possible future — the
  wire format already accepts optional keys per the pinned Contract 3 of
  `metadata-zfs-2026-09`, and the branch is user-owned) or a
  charter-violating server-side join store. Start with the xattr; file the
  upstream wish (`ZFS_EV_PRINCIPAL`, opaque u64/string set by the writer
  process via a new mount option or xattr handshake) as the follow-up that
  would make per-event attribution native.
- **Tombstones:** after DELETE, the object and its xattr are gone; only the
  event log's REMOVE row (and its gateway-uid) remains. Document that
  delete attribution is event-log-only unless/until the upstream branch
  carries a principal field.
- **Reads are unattributed** at the ZFS layer (the xattr stamp is written;
  read attribution would mean stamping on GET, mutating objects on read —
  rejected). If read auditing matters, the gateway's existing request log
  (transient, non-durable) is the place; making it durable is a charter
  decision to escalate, not to work around.
- **Non-ZFS backends:** the same principal stamp belongs in the existing
  per-object `.meta` sidecar JSON (one field) so the audit model is
  backend-uniform; on ZFS the sidecar is never used for history (charter),
  so the xattr is the object-attached twin there.

### What this means for external auth systems

The design treats `AccessKeyID` as the canonical principal string end to end:

- An external IdP/AuthN service (e.g. an OIDC provider fronted by a key
  broker) mints access keys bound to real identities. zeta-object never
  stores identity beyond the key id it is handed; it stamps what it was
  given.
- The object-level xattr (`user.zeta.owner`) then becomes the **audit
  substrate** those systems read back: "who owns/accountable for object Z"
  is answerable from the filesystem alone — by the gateway, by an operator
  with `getfattr`, or by an external auditor reading the dataset directly —
  with no zeta-object database involved. This is the charter-aligned
  property that makes the model worth the xattr cost.
- Bucket-level grants (today's `Identity.BucketGrants`, tomorrow's pluggable
  auth) remain the *enforcement* layer; dataset user properties
  (`zeta:grants`) are available as a host-visible mirror of that policy if
  an external system wants to introspect it with `zfs get`, but enforcement
  never reads them (filesystem user properties have no effect on ZFS
  behavior by definition).

## 3. Change surface (sketch, no code)

| Package | Change |
|---|---|
| `internal/auth` | None for the stamp itself (AccessKeyID already exists). The open "pluggable authentication" issue defines where external IdP mapping lands; this design only consumes `Identity.AccessKeyID`. |
| `internal/backend/fsbackend` | On the write path (Put, multipart commit, Copy): after the data file is in place, `fsetxattr(user.zeta.owner, user.zeta.last_write_op)`. Needs the principal threaded into `PutOptions` (new field, e.g. `Principal string`) — today `Put` has no identity argument, which is the single biggest signature change implied here. Delete path: nothing (tombstone documented). Non-ZFS sidecar path: add the same two fields to `ObjectMetadata`. |
| `internal/backend` | `objectmodel.PutOptions` gains the optional principal field (backend seam change; advisory, backends that cannot persist it ignore it — consistent with Capabilities being advisory but operations reporting errors). |
| `internal/metadata` | No `MetadataProvider` interface change (the seam is frozen and must not gain methods). The *enrichment* reads xattrs at serve time via a new small structural interface (pattern per the frozen-seam rule: type-asserted, compile-time-checked, e.g. `principalReporter` with `Owner(path) (string, bool)`), implemented for fsbackend and surfaced in `?events` responses as an `owner` field joined onto the JSON — provider output stays enrichment-only, and the parity gate still holds because plain-FS buckets without the feature return the identical shape with owner absent. |
| `scripts/zfs-validate/` | **Yes, a new probe**: extend the embedded `checks.py` — PUT an object with a known access key, then assert `getfattr -n user.zeta.owner` on the dataset file matches; assert `?events` still matches `zfs events -j` ground truth (uid column equals the server uid, not the key); assert DELETE leaves no xattr (file gone) and the REMOVE row present. Must run on zfs-meta per the hard validation rule before any implementation lands. |
| e2e | One case under `scripts/e2e/cases/`: PUT → `?events` shows owner field; PUT by a second access key → owner changes (requires multi-identity fixtures the auth-2026-09 tree introduces). |

## 2a. Adopted variant (2026-10-01): additive writer breadcrumbs

Decision on the recommended model, per review: keep the xattr substrate, but
make stamps ADDITIVE and writer-scoped instead of last-writer single-value.

- **Owner is set once, at create.** `user.zeta.owner` = AccessKeyID of the
  creator, written in the same code path as the object's first put. Later
  writers NEVER overwrite it — the object keeps its creator, matching both
  the S3 "object belongs to the bucket account / created by X" intuition and
  POSIX owner immutability.
- **Writers accumulate as per-principal xattrs.** One xattr per principal,
  `user.zeta.writer.<accessKeyID>` (value = last op + timestamp, e.g.
  `put@2026-10-01T22:14Z`). Per-principal NAMES, not one shared list value:
  a shared `user.zeta.writers` list would need get→append→set on every
  write, which races between concurrent writers. Distinct names are
  independent writes — no read-modify-write, no lock, idempotent per
  principal.
- **Read path may reuse the mechanism, best-effort only:** `user.zeta.reader.<accessKeyID>`
  set once per principal (first read stamps, subsequent reads see it present
  and skip — keeps GET write amplification at one xattr per reader, not per
  read). Stamping must be fail-open: on read-only media (snapshot mounts) or
  xattr failure, the GET still succeeds, stamp silently skipped. Never let
  audit metadata break the data path.
- **Dataset guidance:** prefer `xattr=dir` for bucket datasets (no practical
  size/count limit); `xattr=sa` inlines up to ~64K, enough for tens of
  thousands of principals but documented as the limit. Xattr names cap at
  255 bytes — AccessKeyIDs are short; validate at config load that access
  keys fit the prefix budget.
- **Semantics note (attribution ≠ authorization):** the xattrs answer "who
  HAS accessed," not "who MAY access." Access remains the grants layer
  (enforcement); breadcrumbs remain history (audit). A revoked key's
  breadcrumb stays — it records what happened, which is the point of an
  audit trail.
- **Traceback chain:** xattr breadcrumb (AccessKeyID) → identity mapping
  (config.json `identities` today, external IdP/key-broker tomorrow) →
  human user. The breadcrumb names the principal string the gateway was
  given; the mapping that resolves it to a person lives wherever identities
  are defined, not in the filesystem. Breadcrumbs survive identity deletion
  (the key id remains as a pseudonym).
- **Join with the event log, unchanged:** event log = when/what (timeline),
  xattrs = who (attribution), now per-principal with last-op timestamps
  instead of last-writer-only.

## 2b. Clarifications from review (2026-10-01)

### Xattrs are not ACLs — no permission interaction

ACLs (access control lists) are permission entries. On ZFS they live in a
*system-namespace* xattr (`system.posix_acl_access`) that the kernel owns
and enforces. `user.*` xattrs are a separate application namespace with
ZERO effect on permissions, mode bits, or file access — annotation only.
`user.zeta.owner` / `user.zeta.writer.<key>` therefore never overwrite or
disturb existing file permissions or ACL entries; separate namespace,
separate names, no interaction. (The design deliberately does NOT use
ACLs: ACLs enforce, this design records. The "multiple owners" intuition
from ACL entries does not carry over — but the per-principal writer
xattrs reproduce the same outcome additively.)

One real interaction, minor: the `user.*` namespace is writable only by
the file's owner or root (CAP_FOWNER). The gateway owns the files it
writes, so this is a non-issue — and it is exactly why read-path stamping
must be fail-open (a read-only snapshot mount would reject the stamp).

### Read-attribution is a per-bucket tunable

Read-path stamping mutates objects on GET (one xattr per reader,
first-read-only per 2a). On a read-heavy bucket that is extra txgs and
event-log rows, so it ships behind an opt-in knob, per bucket:

- **Phase 1 — config.json** (first implementation): per-bucket option
  `"auditReads": true` alongside existing bucket config. Fail-loud
  parsing, e2e-testable with the existing harness. Off = zero read-path
  overhead.
- **Phase 2 (follow-up) — ZFS user property**: `zfs set
  zeta:audit_reads=on <dataset>`, read by the server at probe time. The
  knob lives on the backing storage (source of truth), survives
  dataset rename/move, and is settable by operators without touching
  server config.

Mechanics in both phases are the section-2a read stamp: first read per
principal writes `user.zeta.reader.<key>`, fail-open, skipped entirely
when the bucket's tunable is off.

## 2c. End state: per-event attribution upstream (`ZFS_EV_PRINCIPAL`)

Filed as **GH issue #7** (bhodgens/zeta-object): add an opaque,
application-supplied principal tag to ZFS event records. Writer registers
the tag once per session (new ioctl — option A, recommended over a
per-event xattr handshake); `zfs_events.c` copies it into every record
alongside `crgetuid()`; zmetad exports it as an optional SCHEMA column
(additive layout bump; older consumers ignore it).

- **Performance:** near-zero at write time — one field copy into the
  already-built record; strictly cheaper than the xattr stamp (which costs
  a `fsetxattr` per write = a real metadata txg + an extra event row), and
  it makes reads and deletes attributable, which no gateway-side mechanism
  can do.
- **Blast radius:** `zfs_events.c`, `zfs_ioctl.c` (option A), the
  in-kernel credential struct, zmetad SCHEMA — all inside the
  extended-metadata branch. **No on-disk pool format change** (the event
  ring is in-RAM; send/recv and imports untouched), so it is safely
  attributable to the extended-metadata capabilities — no separate branch.
- **Migration:** this design's xattr breadcrumbs are the interim and the
  durable per-object summary; once `ZFS_EV_PRINCIPAL` lands, the event log
  becomes the complete per-event audit trail and the `?events` join gains
  a per-event `principal` field.

## References

- [zfsprops.7 — User Properties; xattr; aclmode/aclinherit/acltype](https://openzfs.github.io/openzfs-docs/man/master/7/zfsprops.7.html) (and [v2.4 snapshot](https://openzfs.github.io/openzfs-docs/man/v2.4/7/zfsprops.7.html))
- [zfs-set.8](https://openzfs.github.io/openzfs-docs/man/master/8/zfs-set.8.html)
- [zpool-events.8 / zfs-events payloads](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-events.8.html) — kernel event nvlist fields
- [OpenZFS docs: ACLs and Extended Attributes](https://openzfs.github.io/openzfs-docs/Basic%20Concepts/Datasets/ACLs.html)
- zfs-metadata branch `extended-metadata` @ 9f954271a: `contrib/zmetad/SCHEMA.md` (layout v6 — uid/gid population per op, io-window coalescing, gaps/loss semantics), `module/zfs/zfs_events.c` (`crgetuid` sourcing)
- Repo: `docs/plans/metadata-zfs-2026-09/master.md` (ObjectEvent shape, Contract 3 wire format), `internal/metadata/metadata.go` (frozen seam), `internal/auth/auth.go` (Identity), AGENTS.md charter
