# WebDAV Conformance Instantiation + E2E Case - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Run the reusable frontend conformance suite (`internal/frontend/conformance.go`) against the webdav frontend, and add `scripts/e2e/cases/18-webdav.sh` — curl-based wire coverage of every implemented verb plus the 401 path — satisfying the AGENTS.md same-change e2e rule. Mount-level testing (macOS Finder, davfs2) is documented as a manual procedure, explicitly outside CI.
- **Dependencies:** 02, 03, 04 all landed (full read/write/auth surface exists).
- **Estimated Context:** 70K
- **Concurrency Group:** E (the whole-tree gate)

## Goal

1. `TestWebdavFrontend_Conformance` in `internal/frontend/webdav/` calls
   `frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})` and
   passes — issue acceptance criterion "passes the Frontend conformance suite."
2. `make e2e` includes case 18 proving, over the WIRE against a running
   server: OPTIONS, PROPFIND Depth 0/1, GET/HEAD, PUT, DELETE, MKCOL, COPY,
   MOVE, 401 anonymous, 403 read-only — both bucket modes if the harness
   allows a second private server cheaply (case 16 pattern), else mode B on
   the suite server plus a private mode-A server.
3. The case header comment documents the manual mount procedure (Finder
   `Cmd+K`, `mount_webdav`, davfs2 invocation, expected result set) marked
   MANUAL / CI-SKIPPED with the reason.
4. All pre-existing e2e cases (01-17) remain green.

## Context

Read first:

- `internal/frontend/conformance.go` + `conformance_test.go` — the suite and
  how the s3 frontend consumes it (`grep -rn RunConformanceSuite internal/`)
- `scripts/e2e/lib.sh` — assert helpers (`assert_status`, `assert_contains`,
  `assert_eq`), the `set -u` (never `-e`) contract, real-exit-code counting
- `scripts/e2e/cases/16-frontends-config.sh` — the private-server pattern:
  own workdir, own cert, free port via python3, `trap cleanup EXIT`,
  `launch_expect_fail` for fail-loud checks
- `scripts/e2e/cases/14-custom-bucket.sh` — the second private-server example
- `scripts/e2e/run-e2e.sh` — cases are sourced in lexical order; number 18
  to land after 17
- The suite server exposes S3 with SigV4 credentials; WebDAV speaks Basic,
  so either the webdav frontend shares the default listener (mode B with
  `bucket` set) or gets its own `listenAddr` entry — follow case 16's
  private-server approach for a dedicated instance with a config crafted
  for this case

WebDAV over the wire is plain curl: `-X PROPFIND -H "Depth: 1"
--data-binary @-` with heredoc bodies, `--user user:pass`, `-k` for the
temp cert. No WebDAV client libraries exist in the harness; curl is the
wire-accurate tool. The 207 body is asserted with `assert_contains` on
distinctive fragments (`<D:collection/>`, an href, `getcontentlength`).

## Interface Contracts (From Parent)

- Contract 1: conformance instantiation uses `webdav.New` + the auth tree's
  BasicAuthenticator (test credentials), registered name `"webdav"`.
- Contract 4 wire assertions mirror the error table exactly — the e2e case
  is the enforcement of Contract 4 in a real process.
- AGENTS.md: this case ships in the SAME change as the feature — leaf 05 is
  therefore not optional and blocks tree completion.

## Tasks

### Task 1: Conformance suite instantiation

**Objective:** webdav passes the shared suite.

**Files:**
- Create: `internal/frontend/webdav/conformance_test.go`

**Step 1: Write the test:**

```go
func TestWebdavFrontend_Conformance(t *testing.T) {
    be := newConformanceStubBackend() // reuse or extend the leaf 02/03 stub
    f := webdav.New(be, webdav.Config{Bucket: "conf"}) // mode B: deterministic single tree
    frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}
```

Plus a mode-A variant (`Config{}`) so `Capabilities().Buckets==true` is the
declared-and-proven path. If the suite's capability-gated checks demand
fixes in the frontend (e.g. mountability on `/`), fix the FRONTEND, never
the suite.

**Step 2:** Run — expect pass (02-04 should have kept it healthy); any
failure is a real frontend bug: fix, rerun.

### Task 2: E2E case 18 — read + auth surface

**Files:**
- Create: `scripts/e2e/cases/18-webdav.sh`

**Structure** (copy case 16's skeleton: set -u, mktemp workdir, openssl cert,
free port, private server with webdav config, trap cleanup, real exit codes
via lib.sh asserts):

1. Header comment block: MANUAL MOUNT PROCEDURE, CI-SKIPPED. Contents:
   - macOS: Finder `Cmd+K` → `https://127.0.0.1:PORT/` → accept self-signed
     cert → enter credentials → verify browse/read/write/delete via Finder;
     or `mkdir /tmp/webdav && mount_webdav -v webdav
     https://127.0.0.1:PORT/ /tmp/webdav`
   - Linux: `mount -t davfs2 https://127.0.0.1:PORT/ /mnt/webdav` (davfs2
     config: `use_locks 0` — v1 has no LOCK and rejects it) → verify
     `ls`, `cp`, `rm` through the mount
   - Reason CI skips: needs OS kernel filesystem + interactive cert trust;
     cannot run headless. The curl case below is the CI-level guarantee.
2. Launch private server: config `frontends: [{"type":"webdav",
   "listenAddr":"127.0.0.1:$PORT"}]` (mode A — buckets visible) with temp
   dataDir; seed a bucket + object via the S3 admin path or direct dataDir
   writes (follow how case 15/16 seed state).
3. Assertions (auth first):
   - Anonymous `OPTIONS /` ⇒ 401 with `WWW-Authenticate: Basic` header
   - Bad password `OPTIONS /` ⇒ 401
   - Valid creds `OPTIONS /` ⇒ 200, `DAV: 1` in headers
   - `PROPFIND /` Depth 1 ⇒ 207, body lists the seeded bucket as a collection
   - `PROPFIND /BUCKET/` Depth 1 ⇒ 207 contains seeded object href,
     `getcontentlength`, `getetag`, `getlastmodified`
   - `PROPFIND /` with `Depth: infinity` ⇒ 403
   - `GET /BUCKET/obj` ⇒ 200 + exact body; `HEAD` ⇒ 200 empty
   - `GET /BUCKET/missing` ⇒ 404
4. Then leaf 5b adds the write-surface assertions (below).

### Task 3: E2E case 18 — write surface (same file, part 2)

Continue the case:

- `PUT /BUCKET/e2e18.txt` with body ⇒ 201; `PUT` again ⇒ 204
- `GET /BUCKET/e2e18.txt` round-trips the bytes
- `COPY /BUCKET/e2e18.txt` with `Destination: /BUCKET/e2e18-copy.txt` ⇒ 201
- `MOVE /BUCKET/e2e18-copy.txt` to `/BUCKET/e2e18-moved.txt` ⇒ 201/204;
  old href now 404; new href 200
- `MKCOL /BUCKET/e2e18dir/` ⇒ 201; `PUT /BUCKET/e2e18dir/f.txt` ⇒ 201;
  `PROPFIND /BUCKET/e2e18dir/` Depth 1 lists `f.txt`
- `DELETE /BUCKET/e2e18dir/f.txt` ⇒ 204; `DELETE /BUCKET/e2e18dir/` ⇒ 204;
  `PROPFIND` of the dir ⇒ 404
- `MKCOL /BUCKET/noparent-dir/sub/` (parent absent) ⇒ 409
- Mode B check (cheap): same server, second webdav entry with `bucket`
  set on its own port ⇒ `PROPFIND /` lists the configured bucket's CONTENTS
  (not the bucket itself); 401 still enforced
- Record PASS/FAIL via lib.sh counters; case exits with the counted result

### Task 4: Full gate

```
make e2e          # cases 01-18 ALL green
go test ./... -count=1
make test && make test-race && make test-cover-enforce
make vet && make fmt-check && make lint NEW_FROM_REV=<rev>
```

If aggregate coverage dipped below the floor because the new package's
wiring is untested, extend webdav unit tests (NOT the case) until the floor
holds — and per AGENTS.md, re-measure and update floors in the SAME change
if code moved packages.

## Self-Verification Checklist

- [ ] Conformance suite runs for webdav in BOTH modes; green
- [ ] Case 18 covers all nine verbs + 401 + 403 + mode B, over the wire, real exit codes
- [ ] Manual mount procedure in the case header; explicitly CI-skipped with reason
- [ ] `make e2e` fully green; cases 01-17 untouched and green
- [ ] lib.sh conventions: set -u, no `|| true`, assert helpers only, create/cleanup pairing
- [ ] No credentials/ports hardcoded; free-port + mktemp patterns from case 16
- [ ] Coverage floor holds; no debug artifacts
- [ ] All files at exact specified paths

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Conformance instantiation correct (suite not modified; frontend fixed if needed)
- [ ] Case 18 asserts Contract 4 statuses over the wire — spot-check 3 rows against the table
- [ ] 401/403 wire checks present with header assertions
- [ ] Mount procedure accurate (realm string, no-LOCK davfs2 hint, mount_webdav syntax)
- [ ] Harness hygiene: cleanup trap, sentinel-free, deterministic port selection
- [ ] AGENTS.md rule satisfied: feature ships WITH its e2e case in the same change

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The case seeds state through the S3 side or dataDir, then exercises it
  through WebDAV — that cross-frontend consistency IS the neutral-model
  proof; keep one seeding method (whichever cases 15/16 use).
- curl and `Depth` header case-sensitivity: clients send `Depth: 1`; the
  server must parse case-insensitively — if leaf 02 didn't pin that, add a
  wire assertion with lowercase `depth: 1` here.
- davfs2 sends PROPFIND for every path element on mount and will hang the
  mount if the root response is malformed — the manual procedure doubles as
  the strictest XML validator available; run it once before calling the
  tree complete.
