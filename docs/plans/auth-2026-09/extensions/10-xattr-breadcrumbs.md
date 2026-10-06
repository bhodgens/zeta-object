# Principal Xattr Breadcrumbs + Gateway Audit Log - Implementation Leaf

> **Status:** IMPLEMENTATION LEAF. Design authority:
> [docs/design/zfs-principal-metadata.md](../../../design/zfs-principal-metadata.md)
> sections 2a (adopted additive writer breadcrumbs), 2b (ACL-vs-xattr
> clarification; per-bucket `auditReads` tunable), 2c (`ZFS_EV_PRINCIPAL`
> upstream - explicitly OUT of scope here), and 3 (change-surface table).
> The audit log is the charter exception decided 2026-10-02 (AGENTS.md
> charter text).

## Meta

- **Parent:** [../master.md](../master.md) extension tree (after leaf 09).
- **Scope:** thread the authenticated principal to the fs write path; stamp
  `user.zeta.*` xattrs (owner set-once, per-principal writer breadcrumbs,
  opt-in per-principal reader stamps); add the writer-only SigV4 audit log;
  enrich `?events` with an owner field when the xattr exists.
- **Dependencies:** multi-identity registry (leaf 01/02), rich grants +
  `AuthorizeOp` (leaf 09), zmetad `?events` endpoints.
- **Code is truth:** the design doc's section-3 table skews vs. the actual
  seam; deviations are recorded in "Deviations from the design sketch".

## Goal

The event log records the acting POSIX uid (= the gateway process). It can
never answer "which principal created/touched object Z." This leaf makes the
gateway stamp the authenticated principal into the object's own filesystem
metadata (xattrs, best-effort, charter-clean: object metadata like mode
bits), gives operators a forensic per-request audit log at the auth boundary
(writer-only, never read back by any request path), and surfaces the owner
through `?events` when present.

## Constraints

1. **Charter** (AGENTS.md): xattrs are object metadata - clean. The audit
   log is allowed ONLY as the write-once auth-boundary exception: no code
   path ever reads the file; nothing in the data path or enforcement
   consults it. No queryable server state is created.
2. **Frozen shapes:** `MetadataProvider` interface must NOT gain methods
   (owner enrichment is a type-asserted structural interface).
   `Identity`/`Grant` structs and `IdentityRegistry` methods untouched.
   `PutOptions` gains an OPTIONAL field - additive, existing constructors
   compile unchanged.
3. **Stdlib + existing deps only.** xattr syscalls come from
   `golang.org/x/sys/unix` - already pinned in go.mod (v0.48.0, indirect
   via sftp); this promotes it to a direct require. No new module. Linux +
   darwin both expose `Fsetxattr`/`Fgetxattr`; a build-tag split (the
   `fsdetect_*` pattern) is not needed. Wrapper file:
   `internal/backend/fsbackend/xattr_unix.go` (`//go:build unix`).
4. **Fail-open stamps:** an xattr failure NEVER fails the data operation -
   one WARN line with the xattr NAME (AccessKeyID is log-safe; it already
   appears in dispatch logs) and continue.
5. **No secrets in the audit log or stamps:** principal = AccessKeyID only.
6. **Fail-loud config:** `auditReads` wrong type = startup error;
   `auditLog.path` unwritable at startup = fatal.
7. **Coverage floors / e2e rule:** new user-facing surface (config keys,
   `?events` field, audit behavior) ships with unit tests + e2e case 28 +
   zfs-validate probes in the SAME change.

## Contracts

### Contract 1: principal threading (`objectmodel.PutOptions.Principal`)

```go
// File: internal/objectmodel/model.go — PutOptions gains ONE field:
type PutOptions struct {
    ContentType string
    Metadata    map[string]string
    IfMatch     string
    IfNoneMatch string
    // Principal is the authenticated principal (AccessKeyID) performing
    // the write. Empty = unattributed write (backends skip stamping).
    // Advisory: backends that cannot persist it ignore it.
    Principal string
}
```

- **Owner:** objectmodel (seam). **Consumers:** fsbackend (stamp), s3
  frontend (populates it).
- The dispatch layer publishes the authenticated identity into the request
  context under the ONE shared key defined in `internal/auth`
  (`auth.WithIdentity`, read back with `auth.IdentityFromContext`;
  `dispatch.go`'s `withAuthenticatedIdentity` is a thin alias). It MUST
  stay shared, not per-frontend: webdav once published under its own
  private `identityKey` type, so the shared batch executor resolved no
  principal for a batch arriving over webdav/h3 and stamped
  owner='unauthenticated' (found by the live ZFS harness, s15).
  Write handlers read
  it via `identityOf(r)` and set `opts.Principal = identity.AccessKeyID`:
  - `putObjectHandler` (object_handlers.go:109) - plain PUT.
  - `copyObjectHandler` (object_handlers.go:1409) - the destination Put
    carries the copier as Principal (op `copy`).
  - Multipart: `completeMultipartUploadHandler` assembles the final object
    ABOVE the seam (direct-fs, not fsbackend.Put) - it stamps `op=multipart`
    itself via the same xattr helper (see Contract 2b).
- fsbackend treats Principal as advisory; empty = skip stamping.

### Contract 2: xattr stamps on the fs write path

New file `internal/backend/fsbackend/xattr.go` + `xattr_unix.go`:

```go
// xattr name prefix + names (user.* namespace; annotation only, zero
// effect on permissions - design 2b).
const (
    xattrOwnerPrefix  = "user.zeta.owner"
    xattrWriterPrefix = "user.zeta.writer."   // + AccessKeyID
    xattrReaderPrefix = "user.zeta.reader."   // + AccessKeyID
    // XattrNameBudget is the 255-byte name cap (design 2a). The registry
    // rejects any AccessKeyID that would reach it.
    XattrNameBudget = 255
)

// StampOwner sets user.zeta.owner = principal ONLY when absent
// (create-time; never overwritten afterward). Fail-open.
func stampOwner(path, principal string)

// StampWriter sets user.zeta.writer.<principal> = "op@RFC3339-UTC".
// Per-principal NAME: no read-modify-write of a shared list. Fail-open.
func stampWriter(path, principal, op string)

// StampReaderFirstRead: if fgetxattr(user.zeta.reader.<principal>) says
// present, skip; else set value = last-read RFC3339. One xattr write per
// reader per object. Fail-open. (auditReads-gated by the caller.)
func stampReaderFirstRead(path, principal string)

// ValidateAccessKeyLen errs when prefix+len(keyID) reaches 255
// (registry-side boundary check, design 2a).
```

#### 2a. fsbackend.Put (the seam write path)

After the data write succeeds (`writeFileAtomic(dataPath, ...)` succeeds,
object.go:112) and BEFORE the sidecar write:

- If the data file did NOT exist before this operation (checked with
  `os.Stat` before the write, inside the already-held key lock):
  `stampOwner(dataPath, opts.Principal)`.
- Always: `stampWriter(dataPath, opts.Principal, "put")`.
- Both skipped when `opts.Principal == ""`. Best-effort per Constraint 4.

#### 2b. Above-seam multipart commit

`finalizeComplete` (s3/multipart_handlers.go) stamps the renamed final
object directly: owner-if-absent + `user.zeta.writer.<principal>` with
op `multipart`. The principal arrives via the request context
(`identityOf(r)`); the xattr helpers are implemented once in fsbackend and
exported for this single above-seam call site (`fsbackend.StampWriter` /
`fsbackend.StampOwnerIfAbsent` - exported because the multipart assembly is
pinned ABOVE the seam by master Contract 4). Multipart-complete is a
CREATE-or-overwrite; owner is set only when the final object file did not
exist before the rename.

#### 2c. Copy

`copyObjectHandler`'s destination `b.Put` carries
`Principal: identityOf(r).AccessKeyID`; fsbackend stamps op `copy` on that
path. To distinguish put vs copy without widening PutOptions further, the
s3 handler marks the op via a new optional field... **Decision: not worth a
second field.** fsbackend always stamps op `put` on Put; the COPY
distinction is carried by naming: PutOptions gains only `Principal`
(frozen-minimal). **Final call (deviation from the task sketch, recorded):**
ops are `put` for plain Put and multipart-complete stamps `multipart` in
the frontend; Copy goes through `b.Put` and stamps `put` with the copier as
principal - the owner/writer attribution (WHO) is what the audit model
needs; the op string records the write family. Documented in README.

### Contract 3: per-bucket `auditReads` tunable + read stamping

Config (config.go): the buckets object form (`bucketCfg`) gains
`auditReads` (`json:"auditReads,omitempty"`). Fan-out mirrors `Backend`:
`ServerConfig` gains `BucketAuditReads map[string]bool` (json:"-"),
populated by `bucketsRaw.apply`. Fail-loud: a non-bool value is a JSON
decode error naming the bucket (DisallowUnknownFields already aborts on
typos; the bool type error names the bucket through the existing wrapper).

The s3 frontend learns the tunable through the config view seam
(`ServerConfigView` gains `AuditReads map[string]bool`);
`auditReadsFor(bucket)` consults it (default false). `getObjectHandler`
calls `fsbackend.StampReaderFirstRead(dataPath, principal)` best-effort
when true, AFTER a successful open, using the data path from the sidecar
resolution. Off = zero read-path overhead (no syscall at all).

Wait - the handler does not hold the data path (the backend owns it). The
frontend resolves it handler-side exactly like the existing action-context
plumbing does (`objectDataPathFor(bucketPath, objectName)` +
sidecar-resolved path via the same helper the action context uses; the
plain path is the shadow-aware `objectDataPathFor`). Fail-open makes any
residual path mismatch harmless: worst case the stamp lands on the
canonical path or is skipped.

### Contract 4: gateway audit log (charter-exception layer)

New file `internal/frontend/s3/audit_log.go`:

```go
// AuditLogConfig (JSON key "auditLog", top-level):
//   {"path": "/var/log/zeta/audit.jsonl"}   absent = disabled (default off)
type AuditLog struct {
    Path string `json:"path"`
}

// auditWriter: os.File opened O_APPEND|O_CREATE|O_WRONLY at wiring time;
// *sync.Mutex serializes appends. Best-effort: a failed write logs one
// WARN and never breaks the request. NOTHING ever reads this file.
type auditWriter struct { mu sync.Mutex; f *os.File }

type auditRecord struct {
    TS        string `json:"ts"`        // RFC3339Nano
    Principal string `json:"principal"` // AccessKeyID
    Method    string `json:"method"`
    Bucket    string `json:"bucket"`
    Key       string `json:"key"`
    Op        string `json:"op"`        // auth.Op vocabulary: read|write|list|delete|create
    Status    int    `json:"status"`
    Denied    bool   `json:"denied"`
}
```

- **Seam:** `s3.InstallAuditWriter(w *AuditWriter)` (seam.go hook pattern,
  hookMu-guarded). `nil` = disabled (default; tests unaffected).
- **Wire point:** `serveHTTP` (dispatch.go) records exactly once per
  authenticated request AFTER authentication and AFTER the authorization
  decision:
  - auth failure → `denied:true`, principal = the PRESENTED access key id
    when it parsed (log-safe; else empty), op = `opForMethod`, status =
    the rendered status. (Forensic value: brute-force attempts.)
  - authz denial (`authorizeS3Request` false) → `denied:true`, status 403.
  - success → `denied:false`, status = the handler's response status,
    captured with a wrapping `responseWriter` status recorder installed
    around the dispatch switch.
- Config: `ServerConfig.AuditLog` (`json:"auditLog"` in the UnmarshalJSON
  alias, DisallowUnknownFields still enforced). Startup: when Path != "",
  package main opens the file (creating parent dir best-effort); failure is
  FATAL (fail-loud, Constraint 6). Install into the seam. Reload caveat:
  the audit log is opened at startup only (SIGHUP does not re-open it;
  documented).
- **Charter discipline:** grep-level guarantee - no production code path
  opens this file for reading; the type exposes only `Append(rec)`.

### Contract 5: `?events` owner enrichment (frozen seam respected)

New structural interface (capability_endpoints.go, next to
`detailReporter`):

```go
// principalReporter is the type-asserted enrichment seam: reports the
// object's xattr owner when the backend recorded one. The frozen
// MetadataProvider interface gains NO methods.
type principalReporter interface {
    Owner(path, key string) (string, bool)
}
```

- Implemented by `*fsbackend.FS`: reads the `user.zeta.owner` xattr at the
  object's resolved data path; `(\"\", false)` when absent/unreadable -
  NEVER fabricated. The s3 endpoint type-asserts the bucket's Backend:
  `if pr, ok := b.(principalReporter); ok { owner, found = pr.Owner(...) }`.
- Wire: `objectEventJSON` gains `Owner string \`json:\"owner,omitempty\"\``
  set ONLY on events for objects whose xattr exists (per-object lookup at
  serve time; the field is simply absent otherwise). Parity gate: buckets
  without stamps return the byte-identical pre-change shape (owner absent
  on every event, envelope keys unchanged) - pinned by a unit test.
- The backend is resolved handler-side via `backendFor(bucketName)`.

### Contract 6: fail-loud registry boundary check

`auth.NewMultiRegistry` (registry.go) rejects an AccessKeyID where
`len(fsbackend writer prefix)+len(accessKeyID) >= 255` with an error naming
the identity. To avoid an auth→fsbackend import, the check carries the
computed budget locally: the writer xattr name is
`user.zeta.writer.<keyID>`; a small helper in internal/auth computes
`len(\"user.zeta.writer.\")+len(accessKeyID)` and errors at 255+. (Design
2a: "validate at config load that access keys fit the prefix budget."
Trivially true for real keys; the check documents the boundary.)

## E2E Plan (AGENTS.md hard rule)

New case `scripts/e2e/cases/28-xattr-audit.sh`, private-server pattern
(cases 26/27: own config.json, own port, create/cleanup pairing, `BKT=`
convention, lib.sh assert helpers + s3req). Identities: `ak-one`,
`ak-two`; buckets `e2e28-audit` (plain) and `e2e28-reads` (auditReads
true); `auditLog` path under the case workdir.

- **28a owner stability:** ak-one PUTs obj → `getfattr -n user.zeta.owner`
  = ak-one. ak-two overwrites the same key → owner STILL ak-one;
  `user.zeta.writer.ak-two` exists (value `put@<RFC3339>` shape).
- **28b writer accumulation:** two writers = two distinct
  `user.zeta.writer.*` xattrs on the object.
- **28c reader stamp:** GET as ak-two on the auditReads bucket →
  `user.zeta.reader.ak-two` appears; second GET adds nothing new (value
  unchanged). auditReads-off bucket: no reader xattr ever.
- **28d audit log:** jq every line of the JSONL; assert the shape keys
  (ts/principal/method/bucket/key/op/status/denied); assert a denied:true
  403 line (ak-two PUTs into a bucket it has no grant for), a
  denied:false write line for ak-one's PUT, and that the file grows
  append-only.
- **28e ?events owner:** on the ZFS validation host the owner field is
  covered by the zfs-validate probe; locally (no provider) the case
  asserts the envelope WITHOUT owner matches the pre-change shape on a
  plain bucket (parity).

Unit tests (existing patterns: fsbackend temp dirs + real xattrs; s3
httptest servers):
- fsbackend: owner-set-once (second writer does NOT change owner), writer
  accumulation, empty-Principal = no stamps, fail-open (an xattr write
  failure on a reader-only path leaves the op successful - exercised via a
  directory target), reader first-read-only.
- auth: access-key-length boundary reject names the offender.
- s3: audit line shape + denied:true + disabled-by-default (nil writer →
  no file, no error), ?events owner present/absent (parity).
- config: auditReads object form parses; wrong type fails loud.

## ZFS Validation

Hard gate per AGENTS.md. The embedded `checks.py` in
`scripts/zfs-validate/run-zfs-validation.sh` gains a probe section (after
section 1, before the multipart section so the scratch objects exist while
the dataset is fresh):

- The harness config gains identity `valuser2` (secret `valpass2`, grants
  `*` readwrite) and `auditLog` pointing at
  `$REMOTE_DIR/audit.jsonl`. `probe.py`'s `sign()` is refactored to take
  (ak, sk) so the second identity can sign requests.
- New checks:
  1. PUT /zval/val-owner.txt as valuser → `getfattr -n user.zeta.owner`
     on `/$DATASET/val-owner.txt` == `valuser`.
  2. PUT the same key as valuser2 → owner UNCHANGED (`valuser`);
     `getfattr -n user.zeta.writer.valuser2` present.
  3. GET with events → `?events` JSON for the object carries
     `"owner":"valuser"`.
  4. audit log file exists on the host, every line is valid JSON with the
     eight contract keys, and a line with principal=valuser,
     denied=false exists.
- The pre-existing `?events envelope keys` check asserts the EXACT envelope
  key set `{dataset, recordsLost, ringSwaps, events}` on `mp.bin` - that
  object has stamps too once the probe runs before it, so the envelope
  check stays valid (envelope keys never change; owner lives on EVENTS).
  Event-level `owner` addition does not affect that assertion.
- Run ONCE after all commits; report the actual tally. zfs-meta
  unreachable → report blocked, never fake.

## Risks

| Risk | Mitigation |
|---|---|
| Xattr write amplifies the event log (each stamp = a txg + row) | Owner: once per object; writer: one xattr per principal (overwrite); reader: opt-in, first-read-only per principal |
| `user.*` xattr unsupported (tmpfs? mounted nouser_xattr) | Fail-open WARN; stamps are best-effort by contract |
| Race: two creators concurrently stamp owner | Per-key lock already held in Put (fsLockObject); xattrs set only when absent; worst case the absent-check loses a race and the first writer's owner stays |
| Audit log blocks requests | Mutex-serialized O_APPEND writes are µs-scale; failures never propagate |
| Audit log grows unbounded | Operator concern by charter (ZFS dataset + snapshots); documented in README |
| `?events` shape drift breaks consumers | owner is additive+conditional (omitempty); envelope untouched; parity unit test + zfs-validate check |
| Sibling-worktree contamination | Explicit-path staging only; README staged via revert/apply dance per task instructions |

## Deviations from the design sketch (code-is-truth notes)

1. Design section 3 lists `user.zeta.last_write_op`; the adopted variant
   (2a) replaces it with per-principal `user.zeta.writer.<key>` carrying
   `op@timestamp` - implemented per 2a.
2. Design section 3 proposes the non-ZFS sidecar twin fields; this
   implementation stamps xattrs on ALL filesystems (per the task scope:
   "harmless annotation on plain FS too") - no sidecar schema change.
3. Copy op string: `b.Put` stamps `put` (not `copy`) - PutOptions gains
   only `Principal`; the copy-vs-put distinction is not worth a second
   seam field. Owner/writer attribution is unaffected.
4. The design's "write the stamp in the same code path as the object
   write" is realized as: fsbackend.Put stamps after the data write
   succeeds (before the sidecar write); multipart stamps in
   finalizeComplete after the rename (above-seam assembly is pinned by
   master Contract 4).
5. xattr syscalls via `golang.org/x/sys/unix` promoted to a direct
   dependency (v0.48.0 already in the module graph; no new third-party
   code) - chosen over the build-tag pattern since `//go:build unix`
   covers linux+darwin with one file.

## Open Decisions (for review)

1. Phase-2 `zeta:audit_reads` ZFS user property (design 2b) - follow-up
   leaf, not here.
2. Audit-log rotation/retention tooling - operator-side per charter; a
   future leaf could document a log-rotate recipe.
3. `ZFS_EV_PRINCIPAL` upstream (design 2c, GH #7) - explicitly out of
   scope.
