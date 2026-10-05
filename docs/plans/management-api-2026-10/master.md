# Management API - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 7 leaf documents (flat)
- **Scope:** A secure management API for the whole server: an
  administrative surface authenticated by a TLS CLIENT CERTIFICATE
  (mTLS), served on its OWN LISTENER (loopback by default) in a JSON
  REST shape that is deliberately NOT the S3 wire shape, able to read and
  change server configuration at runtime and to perform bucket
  management operations.

## User decisions (FINAL, 2026-10-04)

1. **Credential:** TLS client certificate. The certificate is the
   administrative identity; it is chosen because the same identity model
   extends to future cluster-wide management (a cluster trusts a CA, not
   a local file).
2. **Configuration model:** runtime store. The API mutates an in-memory
   configuration that is authoritative while the process runs; the
   startup file becomes a snapshot. Settings with no runtime apply path
   are reported honestly as restart-required - never silently "applied".
3. **Exposure:** a separate listener with a different wire shape. It must
   not be exposed on the primary S3 interface, and it binds to loopback
   unless the operator explicitly opts out.
4. **Code placement:** the bucket-management logic moves into a shared
   package used by both the S3 frontend and the management API.
   Rationale: the security boundary is the credential and the
   authorization decision, never the package boundary - both callers run
   in the same process and the same privilege.
5. **Destructive scope:** the API cannot destroy ZFS datasets. Dataset
   destruction stays a filesystem-level operator action. The API may
   delete PLAIN-DIRECTORY buckets only.

## Goal

An operator with the administrative client certificate can, over a
loopback JSON API:

- inspect server state (status, effective configuration, buckets,
  frontends, backends, identities without secrets, metadata provider
  state);
- read configuration, change it at runtime, persist it to the config
  file on demand, and see which changes require a restart;
- perform bucket management (list, inspect, create, delete plain-directory
  buckets, per-bucket tunables);
- trigger administrative operations that exist but have no endpoint today
  (auth identity reload, metadata history purge);
- see every management action in the request audit log, attributed to the
  certificate principal.

Non-goals (explicitly deferred): cluster-wide fan-out of management
commands (the certificate model is chosen to allow it, nothing is built
for it here); dataset-level operations (snapshot, destroy, quota,
properties); a web UI; replacing SIGHUP (it keeps working unchanged).

## Architecture

- **Listener seam (leaf 01).** The frozen `frontend.Frontend` interface
  (`internal/frontend/frontend.go:13-18`) gains NO methods. A new OPTIONAL
  interface, `TLSListenerFrontend`, mirrors the existing
  `NonHTTPFrontend` precedent (`frontend.go:34-44`): the frontend supplies
  its own listen address and its own `*tls.Config`. `main_server.go:61`
  currently calls `ListenAndServeTLS(cert, key)` for every dedicated
  listener, which cannot express client-certificate verification, so the
  dedicated-listener path must serve a pre-built config instead.
- **Credential (leaf 01).** Client certificates are verified against a
  configured CA file with `RequireAndVerifyClientCert`. The principal is
  the certificate Subject Common Name; an optional allow-list narrows
  which CA-signed principals are admitted. No certificate, a wrong
  issuer, or an expired certificate is a 401 with a JSON body and no
  detail leakage.
- **Bucket logic (leaf 03).** The create/delete bucket logic currently
  lives unexported in `internal/frontend/s3` (`bucket_handlers.go:149`,
  `:235`, plus `zfsdatasets_handlers.go`). It moves to a shared package
  with an exported surface; the S3 frontend calls it so its wire behavior
  is unchanged, and the management API calls the same code with the
  delete-a-dataset policy refused.
- **Configuration store (leaf 02).** A main-side store owns the live
  configuration. It reuses the existing fail-loud validation and the
  existing seam installers (`s3_wiring.go:49-125`) to hot-apply what can
  be hot-applied, and it keeps an explicit restart-required list for the
  rest.
- **Charter compliance.** Nothing persistent is added. The store is
  in-memory; the config file is the operator's own file; the audit log
  stays write-only (nothing ever reads it back). No new server-owned
  index or sidecar.
- **Audit.** Management requests append to the existing audit log
  (`internal/frontend/s3/audit_log.go:30-39`) with the existing eight
  keys and a new `op` VALUE of `admin`. The key set is unchanged, so the
  e2e case 28 shape assertion stays valid.

## Interface Contracts

### Contract 1: TLS listener seam (leaf 01, consumed by main)

```go
// internal/frontend/frontend.go (ADD; Frontend itself is untouched):
//
// TLSListenerFrontend is an OPTIONAL extension for frontends whose
// listener needs its own TLS configuration (client-certificate
// verification). A frontend implementing it is served on a dedicated
// named or loopback-bound listener built from TLSConfig(), NOT on the
// shared mux and NOT with the process-wide cert pair.
type TLSListenerFrontend interface {
	Frontend
	// Addr returns this frontend's dedicated listen address from its
	// config ("" => config error, rejected at construction).
	Addr() string
	// TLSConfig returns the listener's TLS configuration. The caller
	// (package main) uses it as-is; ServeTLS must be called with
	// already-built certificate material so these settings survive.
	TLSConfig() (*tls.Config, error)
}
```

- `frontends.go` (`listenerSpec`, `mountFrontends`) and `main_server.go`
  (the dedicated-listener goroutine) carry the config through to the
  listener. A `TLSListenerFrontend` must be REJECTED at startup if
  configured without its own `listenAddr`.
- Loopback guard: the resolved address must be a loopback IP unless the
  frontend entry sets `options.allowNonLoopback = "true"`. A non-loopback
  bind without the option ABORTS startup, naming the address.

### Contract 2: admin frontend (leaf 01, consumed by leaf 04)

```go
// internal/frontend/admin (NEW package):
func New(opts Options) (frontend.Frontend, error)

type Options struct {
	ListenAddr      string // own listener; loopback default
	ClientCAFile    string // REQUIRED: PEM bundle of the trusted CA(s)
	AdminPrincipals []string // optional allow-list of certificate CNs
	AllowNonLoopback bool
	// Route implementations are injected by leaf 04 through a Routes
	// interface so this package owns transport + auth only.
}
```

- `Name()` is `"admin"`; `Capabilities()` declares `Buckets: true` only.
- Factory registration: `frontends.go` `frontendFactories["admin"]`;
  option keys validated fail-loud through the existing `validateOptions`
  helper (`frontends.go:127-139`). Known keys: `clientCAFile`,
  `adminPrincipals`, `allowNonLoopback`.
- Principal extraction: certificate Subject Common Name. An empty CN or a
  principal outside a configured allow-list is rejected with 401.

### Contract 3: runtime configuration store (leaf 02, consumed by leaf 04)

```go
// config_store.go (NEW, package main):
type ConfigStore struct{ /* holds the live *ServerConfig under a RWMutex */ }

func NewConfigStore(cfg *ServerConfig) *ConfigStore
// Snapshot returns a deep copy safe to hand to a request handler.
func (s *ConfigStore) Snapshot() ServerConfig
// Apply validates and applies a partial update; returns the applied keys
// and the keys that require a restart to take effect.
func (s *ConfigStore) Apply(patch ConfigPatch) (applied []string, restartRequired []string, err error)
// Persist writes the current live configuration to the config file
// atomically (temp file + rename, the same technique as
// internal/backend/fsbackend/atomic.go:18). Explicit operator action.
func (s *ConfigStore) Persist(path string) error
```

- Hot-apply set (pinned, applied through the EXISTING installers):
  `identities` (the `reloadIdentityRegistry` path, `config.go:574`),
  `region` (`s3.SetRegion`), `zfs_versioning` and
  `zfs_versioning_reflink_retention`, per-bucket `auditReads` and
  `reflinkRetention` (re-install the config view,
  `s3_wiring.go:51-56`), `zfs_bucket_datasets` (provisioner install).
- Restart-required set (pinned): `dataDir`, `listenAddr`, `certFile`,
  `keyFile`, `frontends`, `backends`, `auditLog`, `zmetad_db_path`,
  `zmetad_binary`. `Apply` records them and reports them; it never claims
  they took effect.
- Validation is the existing fail-loud validation (DisallowUnknownFields,
  unknown backend names, unknown versioning mode). An invalid patch
  changes nothing and returns the validator's named error.
- Secrets: the snapshot handed to a handler MASKS `secretKey` values and
  the env-pair credential. A masked value written back unchanged is a
  no-op, never an overwrite with the literal mask.

### Contract 4: shared bucket manager (leaf 03, consumed by leaf 04)

```go
// internal/bucketmanager (NEW package):
var ErrDatasetBucketNotDeletable = errors.New("bucketmanager: bucket is a ZFS dataset; destroy it on the host")

func Create(ctx context.Context, name string) error
func Delete(ctx context.Context, name string, opts DeleteOptions) error
func Exists(ctx context.Context, name string) (bool, error)
func List(ctx context.Context) ([]BucketInfo, error)

type DeleteOptions struct {
	// AllowDatasetDestroy mirrors the S3 frontend's existing behavior.
	// The management API passes FALSE (user decision 5); the S3 handler
	// passes the value that keeps its current wire behavior.
	AllowDatasetDestroy bool
}
```

- Moved behavior, preserved exactly: bucket-name validation, the
  custom-bucket guards (`bucket_handlers.go:163-175`, `:243-250`), the
  emptiness check including `.uploads` and the `.zfs` exclusion
  (`:279-292`), the `lockObject(bucketPath)` serialization
  (`seam.go:129`), and the ZFS dataset create/destroy branch.
- The frozen `internal/backend/backend.go` and
  `internal/metadata/metadata.go` seams are untouched.
- AGENTS.md rule: this leaf MOVES code between packages, so it must
  re-measure and update the coverage floors in the same change.

### Contract 5: management routes (leaf 04)

JSON only, never the S3 XML shape. Every response is
`application/json`. Errors are
`{"error":{"code":"...","message":"..."}}` with an HTTP status.

| Method | Path | Behavior |
|---|---|---|
| GET | `/status` | server version, uptime, listeners, frontends, backends, restart-required keys, metadata provider availability (honest "unavailable" + reason, never invented) |
| GET | `/config` | full effective configuration, secrets masked, plus per-key source and the restart-required list |
| PUT | `/config` | partial update through the store; 200 with `applied` and `restartRequired`; 400 with the validator error; nothing changes on error |
| POST | `/config/save` | persist the live config to the config file atomically |
| POST | `/auth/reload` | re-run the identity reload path (same code as SIGHUP) |
| GET | `/buckets` | list buckets with backend and per-bucket tunables |
| POST | `/buckets` | create (name in the body) |
| GET | `/buckets/{name}` | one bucket: backend, tunables, whether it is a dataset, object count is NOT reported (no index exists; do not invent) |
| DELETE | `/buckets/{name}` | delete; plain-directory buckets only; a dataset-backed bucket answers 409 with `ErrDatasetBucketNotDeletable` |
| PUT | `/buckets/{name}/settings` | per-bucket tunables (auditReads, reflinkRetention) |
| POST | `/purge` | metadata history purge for a named dataset (the operator-only capability that has no endpoint today, README:599-615); body `{"dataset":"pool/ds"}`; gated on the admin tier |

- Every route requires a verified client certificate. There is no
  unauthenticated route, not even `/status`.
- Every route appends one audit record with `op = "admin"`.

### Contract 6: audit operation value (leaf 04)

- `auth.Op` gains `OpAdmin Op = "admin"` and `opVocabulary` accepts it
  (`internal/auth/richgrants.go:30-48`). The bucket-grant model is
  UNCHANGED: `Admin` is never granted through `BucketGrants`, and
  `AuthorizeOp` behavior for the five existing ops is byte-identical
  (a bucket grant must never confer administrative authority).
- `auditRecord.Op` accepts the new value; the eight-key record shape
  (`audit_log.go:30-39`) is unchanged, so the e2e case 28 key-set
  assertion keeps passing.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-admin-frontend-mtls.md | leaf | none | 70K | A |
| 02 | 02-runtime-config-store.md | leaf | none (runs after 03: both edit main.go) | 60K | B |
| 03 | 03-shared-bucket-manager.md | leaf | none | 70K | A |
| 04 | 04-admin-routes.md | leaf | 01, 02, 03 | 70K | C |
| 05 | 05-docs.md | leaf | 04 (behavior) | 40K | D |
| 06 | 06-e2e-case-35.md | leaf | 04 | 50K | D |
| 07 | 07-live-validation.md | leaf | 04, 06 | 40K | E (live host) |

**Concurrency groups:** A: 01 and 03 simultaneously (disjoint files: 01
owns `internal/frontend/frontend.go`, `frontends.go`, `main_server.go`
and the new `internal/frontend/admin` package; 03 owns `main.go`,
`internal/bucketmanager`, `internal/fslock` and the `internal/frontend/s3`
extraction). B: 02 (it also edits `main.go` and `s3_wiring.go`, so it
must FOLLOW 03 in the same worktree). C: 04 (needs 01, 02, 03 committed).
D: 05 and 06 in parallel after 04 (docs and the e2e case touch disjoint
files). E: 07 last, on zfs-meta.

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group A

Dispatch 01 and 03 simultaneously via `delegate_task` (02 belongs to
group B, below):

1. **Read** the leaf, paste the FULL leaf text plus the contracts it
   consumes and produces. Include the repo Coding Conventions block
   below. Include: "Do NOT commit. Do NOT run git add."
2. DO-NOT-TOUCH list (sibling WIP can be dirty in the worktree):
   `go.sum`, `zeta-object-server`, `scripts/zfs-validate/run-zfs-validation.sh`,
   `internal/frontend/s3/reflinkversions.go`,
   `internal/frontend/s3/versioning_handlers.go`. Stage explicit paths
   only.
3. Leaf 03 additionally owns `internal/frontend/s3/bucket_handlers.go`;
   leaf 01 owns `frontends.go` and `main_server.go`. No other leaf may
   touch those files in group A.

### Phase 2: Review and Commit Each Child

After each agent returns, the orchestrator reviews in-session:

1. Read the changed files; check against the leaf spec and contracts.
2. RE-RUN the gates in the parent (subagent gate reports are
   self-claims): `go test ./... -count=1`, `make lint
   NEW_FROM_REV=HEAD` (0 findings), `gofmt -l` clean on changed files.
3. Commit explicit paths only (never `git add -A`).
4. Gaps -> re-dispatch with findings; max 3 cycles, then escalate.

### Phase 3: Dispatch Group B, then C, then D, then E

- Leaf 02 after 03 is committed (both edit `main.go` and `s3_wiring.go`).
- Leaf 04 after 01, 02, 03 are COMMITTED (it wires all three).
- Leaves 05 and 06 after 04 is committed.
- Leaf 07 last: live host. Before dispatching, check whether a sibling
  session owns uncommitted hunks in
  `scripts/zfs-validate/run-zfs-validation.sh`; if so, WAIT or do it
  in-session (highest-collision file in the repo).

### Phase 4: Integration Review

1. Full gates: `make test && make lint NEW_FROM_REV=HEAD && make e2e && make parity-test`.
2. `grep -rcE '^\s+[0-9]+\|' --include='*.go' .` returns zero
   (no line-number corruption).
3. Frozen seams untouched: `git diff --stat` must not list
   `internal/backend/backend.go` or `internal/metadata/metadata.go`.
4. Update the tracking table; report.

## Review Checklist

- [ ] All tasks from each leaf document are implemented
- [ ] Contracts 1-6 satisfied exactly (interface names, option keys,
      route paths, JSON error shape, the `admin` op value)
- [ ] `internal/backend/backend.go` and `internal/metadata/metadata.go`
      UNTOUCHED
- [ ] `internal/frontend/frontend.go` Frontend interface UNTOUCHED
      (only the additive TLSListenerFrontend)
- [ ] No route served without a verified client certificate
- [ ] Loopback guard enforced (non-loopback bind aborts without the
      explicit option)
- [ ] No ZFS dataset is destroyed by any management API path
- [ ] Secrets never appear in any response body or log line
- [ ] S3 wire behavior byte-identical (existing s3 tests unmodified)
- [ ] Audit record key set unchanged; `op` accepts `admin`
- [ ] Tests written and passing (TDD followed)
- [ ] Coverage floors re-measured if code moved packages
- [ ] No scope creep, no debug artifacts, no line-number corruption

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go (repo go.mod version); exported PascalCase,
  unexported camelCase; imports grouped stdlib / third-party / local.
- **Errors:** wrap with `%w`; sentinel vars for wire-mapped conditions;
  no `panic` in library paths; ignored-error and empty-branch forms are
  HOOK-BANNED (`_ = err`, `if err != nil {}`).
- **Go 1.26 modernize forms:** `errors.AsType[T]`, `new(expr)`,
  `for i := range n`, `wg.Go`.
- **Testing:** table-driven; seams are package-level vars the tests
  replace; tests live in the package under test.
- **Formatting:** `gofmt` before reporting; `make lint
  NEW_FROM_REV=HEAD` must be 0 (bare `make lint` shows sibling
  findings).
- **Static binary:** NO cgo, ever.
- **Secrets:** never log a secret; never return one in a body; mask on
  read.

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-admin-frontend-mtls | COMPLETE | 1 | c79dd5a; parent re-verified gates; +CertFile/KeyFile on Options (deviation) |
| 02-runtime-config-store | COMPLETE | 1 | committed; 12 store tests; child bug claim #6 disproved by parent grep (config.go:240/:347) |
| 03-shared-bucket-manager | COMPLETE | 1 | e99093e; moved-block evidence reviewed; floors re-measured; one lock table verified |
| 04-admin-routes | COMPLETE | 1 | committed; parent verified 401-per-route, OpAdmin ungrantable, AllowDatasetDestroy false, 8 audit keys |
| 05-docs | COMPLETE | 1 | a9cea74; docs checked against code, 0 em-dashes, example parses |
| 06-e2e-case-35 | COMPLETE | 1 | b55b469; parent re-ran make e2e: 782/0, case 35 PASS 32 asserts |
| 07-live-validation | COMPLETE | 1 | 140/140 PASS exit 0 first run; dataset-refusal proven on real ZFS with zfs list evidence; no server bugs |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. `make test` - full unit suite green (new: TLS-config construction,
   certificate verification matrix, config store apply/persist, bucket
   manager, route handlers).
2. `make lint NEW_FROM_REV=HEAD` - 0 findings.
3. `make e2e` - full suite green; case 35 runs with generated
   certificates and SKIPS gracefully where `openssl` is unavailable.
4. `make parity-test` - metadata parity untouched.
5. Live: leaf 07's harness section on zfs-meta - mTLS handshake against
   the real server, bucket create/delete on plain directories, and the
   dataset-bucket delete refusal; ALL checks green including
   pre-existing sections; exact tally reported.

## Structural Completeness Check (Before Dispatch)

Run: `python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans --strict-leaves`
(against `docs/plans` - the PARENT; a flat-leaf tree scanned alone is
misread as a forest root).

## Known limitations (v1, discovered during execution)

- **Per-bucket tunables apply only to config-declared buckets.** `PUT
  /buckets/{name}/settings` (auditReads, reflinkRetention) works for a
  bucket that appears in the config `buckets` map. An auto-provisioned
  bucket cannot take per-bucket tunables without a restart-required
  layout change, because the store's buckets-patch path treats an entry
  without a path/backend as a fail-loud parse error. Documented in
  `adminBucketSettingsService`; leaf 05 must state it in the README.
- **Purge takes a dataset name** and passes it to the frozen
  `MetadataProvider.Purge` bucket-path signature; the provider resolves
  it. If the provider is unavailable the route answers with the
  provider's honest reason.

## Open Questions

- None blocking. The four scoping decisions were made by the user
  2026-10-04 (see User decisions above).
- Deferred (not in scope): cluster-wide management fan-out; dataset-level
  operations; a web UI; replacing SIGHUP; OAuth/OIDC credentials.

## Notes

- **Sibling WIP is live in this worktree.** `git status` at authoring
  time shows dirty: `go.sum` and the `zeta-object-server` binary. Stage
  explicit paths; verify `git diff --cached` hunk-by-hunk before every
  commit; never sweep sibling files.
- **Loopback-only is a security control, not a convenience.** The admin
  listener speaks with full authority; binding it to a public address is
  an explicit operator decision that needs the option set.
- **The mux rule matters:** `mountFrontends` (`frontends.go:322-343`)
  rejects a second shared mount, so an admin frontend configured without
  its own `listenAddr` must fail loudly at startup with a message naming
  the rule.
- **Purge is audit-flavored** (`README.md:609-615`): it destroys event
  history plus the permanent gap/loss record. It is exposed here because
  the API is the admin tier that README anticipated, and it is the only
  route that destroys anything irreversibly; docs must say so plainly.
- **Cluster future:** the certificate principal (CN) is the identity
  recorded in the audit log, so a future cluster that shares the CA can
  attribute management actions without redesigning the credential.
