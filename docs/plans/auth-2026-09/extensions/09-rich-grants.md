# Richer Grants: Prefix- and Operation-Scoped, Optionally Time-Bound - Design Leaf

> **Status:** DESIGN ONLY — no code in this leaf.

## Meta

- **Parent:** [master.md](../master.md) (post-completion extension)
- **Scope:** A structured grant expression — per-bucket **key-prefix
  scoping**, **operation scoping** (read/write/list/delete…), and optional
  **time windows** (`not-before` / `not-after`) — parsed fail-loud at config
  load and enforced in exactly one place, while leaving every frozen v1
  contract untouched.
- **Dependencies:** landed leaf 01 core (`IdentityConfig`, `MultiRegistry`,
  `parseGrants`, `CanRead`/`CanWrite`), leaf 02/03 enforcement surfaces
  (`AuthorizeRequest`, `grantedBucketFilter`), SFTP CriticalOptions
  round-trip.
- **Estimated Context:** 60K

## Goal

Today grants are bucket-level and read/write only:

- Config shape: `"grants": {"*": "readwrite", "photos": "readonly"}` —
  `map[string]string` (registry.go:28, `IdentityConfig.Grants`).
- Parsed by `parseGrants` (registry.go:149-176): value must be
  `"readonly"`/`"readwrite"` (frozen vocabulary, registry.go:52-55); absent
  grants ⇒ wildcard readwrite (registry.go:150-152).
- Decided by `CanRead`/`CanWrite` (grants.go:11-28): a present per-bucket
  grant is authoritative; only an absent entry falls through to `"*"`.
- Enforced for S3 via `AuthorizeRequest` (authz.go:41-60) at bucket level,
  plus ListBuckets filtering via `grantedBucketFilter` (dispatch.go:193-209),
  and for FTP/SFTP via the same `Identity` methods (ftp/auth.go, sftp
  driver.go:386).

There is no way to say "this CI key may write only under `photos/2024/`",
"this key may read but never delete", or "this contractor key expires
Friday". This leaf adds all three dimensions in **one grant expression
shape**, parsed once at load.

## Constraints

1. **Frozen v1 shapes** (master.md Notes): `Identity{AccessKeyID string;
   BucketGrants map[string]Grant}` (auth.go:12-15) and
   `Grant{Read, Write bool}` (auth.go:17-20) — **the struct and the map
   key/value types cannot change**. Two plan trees pin them. Everything
   richer must compile down to these two types or live *beside* them.
2. **Single-decision rule** (master.md:103-107): grant logic only in
   `internal/auth`; frontends translate wire formats only.
3. **Backward compatibility:** existing `grants` JSON (string values) must
   parse identically; configs without new keys behave byte-for-byte as
   today (migration contract pattern, master.md Contract 2).
4. **Fail-loud config validation** (registry.go:86-92 precedent): unknown
   ops, bad timestamps, malformed prefixes abort startup naming the offender.
5. **Stdlib only** (time/format parsing incl. RFC 3339 is stdlib).
6. **SFTP CriticalOptions round-trip must stay lossless** for the v1
   subset it carries (sshserver.go:96-100, 133-153 render/parse
   `bucket=rw` pairs) — see Interaction 3 below.

## Design

### Contract 1: the grant expression shape (config surface)

Two forms per entry; a string is legacy shorthand for the object form:

```jsonc
"identities": [
  {
    "name": "ci-bot",
    "accessKey": "AKIDZETACIBOT01",
    "secretKey": "…",
    "grants": {
      // LEGACY (frozen): string value — parses to {ops:["read","write"]}
      "*": "readwrite",
      "photos": "readonly",

      // NEW (object form): same map key space, object value
      "photos/2024/*": {
        "ops": ["read", "write", "list"],   // required, non-empty; subset of
                                            // read|write|list|delete|create|
                                            // (see vocabulary below)
        "not-before": "2026-09-01T00:00:00Z",  // optional, RFC 3339
        "not-after":  "2026-12-31T23:59:59Z"   // optional, RFC 3339
      }
    }
  }
]
```

JSON decoding rule: value decodes as `json.RawMessage` first; if it
unmarshals to a string → legacy path (frozen vocabulary check unchanged);
if an object → rich path. This makes the change **purely additive** to the
frozen `IdentityConfig` JSON: the `Grants` field type changes from
`map[string]string` to `map[string]json.RawMessage`-equivalent *internally*,
but every currently-valid document parses to identical semantics. (Field
type change is an internal, compile-checked migration of one struct in
`internal/auth` — allowed, since `IdentityConfig` is config-side, not one
of the pinned v1 wire shapes; the pinned shapes are `Identity`/`Grant`,
which do not change.)

### Contract 2: internal types (new file: internal/auth/richgrants.go)

```go
// File: internal/auth/richgrants.go (new)
package auth

// Op is a grant operation. The vocabulary is deliberately the S3 verb
// families the frontends already dispatch on; frontends map wire verbs
// (S3 method+subresource, FTP/SFTP commands) onto these in ONE adapter
// table each.
type Op string
const (
    OpRead   Op = "read"    // GET object (+ HEAD); GET bucket sub-resources that read data
    OpWrite  Op = "write"   // PUT/POST object data, multipart upload parts
    OpList   Op = "list"    // listing operations (ListObjects*, ListMultipartUploads)
    OpDelete Op = "delete"  // DELETE object/batch-delete/abort-multipart
    OpCreate Op = "create"  // CreateBucket (+ initiate multipart, PUT bucket sub-resources)
)

// GrantExpr is one parsed grant entry: a key pattern (bucket or
// bucket/prefix-pattern) plus scoping.
type GrantExpr struct {
    Pattern   string    // as written in config, e.g. "photos", "photos/2024/*", "*"
    Ops       map[Op]bool
    NotBefore time.Time // zero = unbounded
    NotAfter  time.Time // zero = unbounded
}

// ParseGrantValue decodes ONE grants map value (string or object form)
// against the identity name for error messages. Fail-loud: unknown op,
// not-after <= not-before, unparsable RFC 3339, empty ops array, unknown
// JSON keys → error naming the offender.
func ParseGrantValue(identity string, key string, raw json.RawMessage) (GrantExpr, error)

// MatchesObject reports whether the entry's pattern covers an object key
// inside bucket. Pattern rules (fail-loud at load, cheap at request time):
//   "*"                     → any key in any bucket (v1 wildcard)
//   "<bucket>"              → any key in that bucket (v1 exact bucket)
//   "<bucket>/<prefix>*"    → keys whose remainder after the longest
//                             common '/'-boundary prefix matches; the
//                             trailing "*" is the ONLY wildcard character
//                             (no mid-string globs, no "**")
func (e GrantExpr) MatchesObject(bucket, key string) bool

// ActiveAt reports whether the time window (if any) contains t (UTC).
func (e GrantExpr) ActiveAt(t time.Time) bool

// Allows is the op check: e.Ops[op].
func (e GrantExpr) Allows(op Op) bool
```

### Contract 3: the frozen-struct bridge — how richer grants compile DOWN to `Grant{Read, Write}`

This is the crux. `Identity.BucketGrants` is `map[string]Grant` and cannot
change, yet rich grants need ops + prefix + time. Resolution: **the frozen
map holds the SAFE FLOOR (v1 semantics, time-filtered); the rich table
lives beside it and only ever *narrows or extends* within what the floor
already answers true.**

```go
// Added to Identity (methods only — struct untouched):
// RichGrants returns the parsed rich entries for this identity; nil for
// identities built without any object-form grant (env pair, legacy-only
// configs, dev mode, SFTP CriticalOptions round-trip) — nil ⇒ pure v1
// behavior, byte-identical.
func (id Identity) RichGrants() []GrantExpr

// WithRichGrants returns a copy of id carrying the rich table (build-time
// composition; Identity stays a value type).
func (id Identity) WithRichGrants(exprs []GrantExpr) Identity
```

Mechanics at registry build time (`parseGrants`, registry.go:149):

1. Split entries into legacy (string) and rich (object).
2. Build the frozen `map[string]Grant` **from the legacy entries only**,
   exactly as today (absent ⇒ `{"*": rw}` fallback — but only when there
   are no rich entries either; see below).
3. Build `[]GrantExpr` from the rich entries.
4. **Floor computation:** for every rich entry, derive its implied v1 floor
   and take the UNION into the frozen map, so `CanRead`/`CanWrite`
   (grants.go) never under-answer for a prefix/ops/timely grant:
   - ops ⊇ {read} ⇒ floor gets `Read: true` on the entry's bucket key
     (`photos/2024/*` contributes to `photos`).
   - ops ⊇ {write} ⇒ floor `Write: true` (write ⊃ read per master
     Contract 1, so both bits).
   - entries with a **not-yet-active** window or **no read/write ops**
     (list-only, delete-only) contribute NOTHING to the floor — the frozen
     map would otherwise answer "can read" outside the window or for an op
     the identity doesn't hold. Time-scoped read/write grants are instead
     carried as *restrictions* (see enforcement below): the floor must
     never grant MORE than the rich table, so a time-bound read grant
     contributes a **zero-bit marker** `Grant{}` under its bucket ONLY when
     no other entry already grants that bucket read — a present
     authoritative entry (grants.go:12-14) that denies by default prevents
     the absent-entry `"*"` fall-through from leaking v1 wildcard access
     over a time-scoped bucket.

Enforcement lives in exactly one new place, wrapped around the existing
decision (not replacing it):

```go
// internal/auth/richgrants.go — THE single rich-grant decision:
// AuthorizeOp answers the full question: may id perform op on
// (bucket, key) at time now?
//   - A nil rich table ⇒ exactly legacy semantics:
//       op read  → id.CanRead(bucket);  op write → id.CanWrite(bucket);
//       op list/delete/create → mapped to CanRead/CanWrite per the
//       adapter table (delete/create ⇒ CanWrite; list ⇒ CanRead),
//       matching what bucket-level v1 grants could express.
//   - With a rich table: allowed iff SOME entry e satisfies
//       e.MatchesObject(bucket, key) && e.ActiveAt(now) && e.Allows(op).
//     v1-method answers are AND-ed with the rich table only in the
//     negative direction (a CanRead/CanWrite false always denies —
//     preserved as a cheap pre-filter for frontend compat).
func AuthorizeOp(id Identity, op Op, bucket, key string, now time.Time) bool
```

Frontend integration (translation only, per the single-decision rule):

- **S3** (`dispatch.go`): the per-request grant checks that currently call
  `AuthorizeRequest(id, bucket, write)` (authz.go:41) additionally pass
  method + object key so the call site can use `AuthorizeOp`; bucket-level
  and service-level paths (ListBuckets via `grantedBucketFilter`,
  dispatch.go:193) stay bucket-scoped: list ops consult entries whose
  pattern matches the bucket (prefix `*` suffix irrelevant for listing the
  bucket root; listing with a `prefix=` query param MAY be filtered by the
  rich pattern — noted as a review decision).
- **`internal/frontend` authz.go**: `AuthorizeRequest` keeps its exact
  signature and behavior (frozen-ish shared helper, used by WebDAV/ownCloud);
  it becomes a thin special case of `AuthorizeOp` with
  `op = read|write, key = "", now = time.Now()` — the one-line delegation
  keeps all HTTP frontends coherent without touching their call sites.
- **FTP/SFTP**: map commands in one adapter table each (RETR/STOR/ LIST/
  DELE/MKD…) to `Op`, then call `AuthorizeOp` with the resolved object key.
- **SFTP CriticalOptions** (sshserver.go:96-100): the v1 render/parse
  (`bucket=rw`) keeps round-tripping the **floor** map losslessly. The rich
  table is NOT serialized through CriticalOptions; instead the session
  handler receives identity accessKeyID and re-resolves rich grants from
  the registry at session start (one lookup, cached for the session —
  same bounded-staleness property as today's handshake-time auth). A
  legacy-only identity produces zero rich entries ⇒ identical wire bytes.

### Time semantics

- `now` is injected, defaulting to `time.Now().UTC()` at the enforcement
  boundary; unit tests pin boundary inclusivity: `not-before ≤ now ≤
  not-after` (inclusive both ends, documented).
- Expired-but-present grants deny — they are NOT stripped at load, so a
  `not-after` in the past is legal config that simply never matches
  (operators may pre-stage future grants; `not-before` in the future is
  the "activate later" idiom).
- Load-time validation rejects only `not-after ≤ not-before` and
  unparsable timestamps — never a window merely because it's expired.

### Backward compatibility proof points

- String-form grants parse via the unchanged frozen vocabulary path;
  `registry_test.go` / `grants_test.go` cases must pass unmodified.
- No grants at all ⇒ wildcard rw (registry.go:150-152) — unchanged.
- Env pair (`auth.EnvPair`, config.go:376) and dev mode (`WildcardIdentity`,
  grants.go:34) build pure-v1 identities with nil rich tables.
- `grantedBucketFilter` (dispatch.go:193) reads only the frozen map →
  ListBuckets shows the floor union; a prefix-only identity appears to have
  bucket-level read for listing purposes but object writes outside its
  prefix are denied by `AuthorizeOp`. Documented as the v1-filtered-list
  limitation (key-level visibility inside listings is out of scope).

## E2E Plan (AGENTS.md hard rule)

New case `scripts/e2e/cases/27-rich-grants.sh` (private-server pattern,
own config.json/port):

- **27a legacy coexistence:** same config carries a legacy string grant and
  a rich object grant; both identities behave per v1 and per rich
  semantics respectively.
- **27b prefix scoping:** identity with `"photos/2024/*": {"ops":["read",
  "write"]}` — PUT `photos/2024/a.jpg` succeeds; PUT `photos/2023/x.jpg`
  → 403 AccessDenied; GET under prefix succeeds.
- **27c op scoping:** readonly-op identity (`"ops":["read","list"]`) — GET
  succeeds, DELETE → 403.
- **27d time window:** `not-after` in the past → GET → 403;
  `not-before` in the future → 403; window containing now → success
  (assert with realistic margins; no clock mocking at e2e level).
- **27e fail-loud validation:** unknown op, `not-after < not-before`,
  empty `ops` → `launch_expect_fail` naming the offender
  (scripts/e2e/lib.sh:122 pattern, mirroring case 18f).
- **27f SFTP floor round-trip:** an SFTP login for a rich-grant identity
  authenticates and the session enforces the floor at minimum
  (extends case 21's pattern).

Unit tests: `internal/auth/richgrants_test.go` — table-driven over
`ParseGrantValue` (both forms, every failure mode), `MatchesObject`
(bucket exact, prefix boundary cases: `photos/2024/*` vs `photos/20240/`,
`photos/2024` without slash must NOT match — boundary is '/'),
`ActiveAt` (inclusive edges), `AuthorizeOp` (nil-table ⇒ v1 equivalence
golden table vs `CanRead`/`CanWrite`; positive/negative/union cases),
floor-union derivation, and the existing suites green unmodified.

## ZFS Validation

The S3 dispatch change touches the request path but not backend/metadata
behavior. Per AGENTS.md scoping this is not a data-plane change; the
implementing change should still run `run-zfs-validation.sh` once as a
regression sweep (same posture as 08-key-rotation.md).

## Risks

| Risk | Mitigation |
|---|---|
| Floor-union drift: frozen map answers MORE than rich table intends | One derivation function, unit-tested against a golden table; the zero-bit marker rule for non-read/write and inactive entries; review checklist item |
| Prefix matching surprises (path traversal, double slash, case) | Pattern grammar is minimal (`trailing '*' only`, '/'-boundary); key normalization follows the object-model's existing key rules — no new normalization invented; reject `*` anywhere but trailing at load |
| Clock dependence in authorization (tests flake near midnight) | `now` injected everywhere; only e2e uses wall clock with wide margins |
| Two enforcement paths drift (AuthorizeRequest vs AuthorizeOp) | AuthorizeRequest becomes a delegating special case; grep check added to review checklist: no CanRead/CanWrite calls left in frontends outside authz.go/driver mapping tables |
| SFTP sessions outlive grant expiry | Same bounded model as today (auth at handshake); documented; rich table re-resolved only at session start |
| Config type change ripples to package main | `IdentityConfig.Grants` is the only touched config struct; `config_identities_test.go` golden tests prove the legacy documents parse identically |

## Open Decisions (for review)

1. Should `list` respect a `prefix=` query parameter at the S3 listing
   endpoints (key-visible scoping inside listings), or stay
   bucket-scoped-as-floor (recommended first cut; narrower scoping is a
   follow-up)?
2. Op vocabulary completeness vs `create` (CreateBucket) — include or fold
   into `write` for v0 of the rich surface? (Recommended: ship the five-op
   vocabulary; the adapter table makes adding ops cheap.)
3. Whether rich grants should be allowed on the `"*"` key in object form
   (time-bounded global grants are a legitimate ops use case; recommended:
   allow, with the same floor rules).
