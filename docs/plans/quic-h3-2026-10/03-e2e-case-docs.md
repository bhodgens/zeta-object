# E2E Case 37 (h3probe) + Docs - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD where applicable (the probe is
> testable via `go run` in the harness; the docs are copy). Do NOT
> commit - the orchestrator handles all git operations after review.
> Do NOT use read_file on existing source files - explore with
> search_files or terminal cat. After completing, report what you
> built, what files you touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the wire-level e2e case exercising WebDAV Range and the
  h3 transport (Go probe client), plus README and
  `docs/protocol-compatibility.md` updates.
- **Dependencies:** leaf 01 (Range semantics, committed) and leaf 02
  (h3 frontend, committed - its report pins the quic-go version and
  API shapes; READ the version from `go.mod` and use the same
  library in the probe).
- **Estimated Context:** 60K
- **Concurrency Group:** B

## Goal

`make e2e` runs case 37: a server configured with webdav (TCP), s3
(default-mount entry), and h3 frontends; the probe PUTs files, GETs
them over HTTP/3, exercises Range (206), asserts auth rejection, and
verifies the Alt-Svc advertisement over TCP. Docs state honestly what
is implemented and what degrades to what.

## Context

- Harness conventions: `make e2e` wraps `scripts/e2e/run-e2e.sh`;
  cases are `scripts/e2e/cases/*.sh` in lexical order; the `BKT=`
  bucket convention and create/cleanup pairing; assert helpers from
  `scripts/e2e/lib.sh`. READ one recent case end-to-end first (case
  31 `31-webdav-lock.sh` exercises webdav; case 35
  `35-management-api.sh` generates certificates) and follow the newest
  shape.
- The config for the case needs an EXPLICIT frontends array: adding
  `h3` means the array must also carry explicit `s3` and `webdav`
  entries (an explicit list disables the default S3 mount - repo
  skill rule). Copy the harness's config-generation pattern from an
  existing case and add the h3 entry.
- The probe lives at `scripts/e2e/h3probe/main.go` (package main,
  module-internal - check how the harness invokes Go helpers; if the
  repo module cannot hold a second main package under scripts/,
  place it at `scripts/e2e/h3probe/` with its own go.mod requiring
  the SAME pinned quic-go version from the repo's go.mod, and say so
  in the report; prefer the in-module location if it builds).
- Stock curl rarely has HTTP/3 enabled; that is why the probe is Go.
  The TCP-side asserts (Alt-Svc header, webdav Range over plain
  HTTPS) CAN use curl if the case's existing curl usage pattern
  supports headers; otherwise extend the probe with a `-transport
  tcp` mode. Prefer ONE probe binary with a mode flag over two tools.

## Interface Contracts (From Parent)

### Contract 5: e2e + docs (you produce this)

- Case file `scripts/e2e/cases/37-h3-webdav.sh` following harness
  conventions: `BKT=` bucket convention, create/cleanup pairing,
  lib.sh assert helpers, graceful skip ONLY where a prerequisite is
  genuinely absent (go is always present - the harness builds the
  server - so the case should always run; do not add a soft skip
  path that hides real failures).
- The probe asserts, over HTTP/3 (mode `h3`):
  1. unauthenticated GET is 401 (Basic auth enforced over QUIC);
  2. authenticated PUT of a file (multi-KB content with known bytes)
     succeeds (201/204 per the webdav wire);
  3. authenticated GET round-trips the exact bytes;
  4. `Range: bytes=0-99` GET is 206 with `Content-Range:
     bytes 0-99/<size>` and exactly the first 100 bytes;
  5. a suffix-range GET (`bytes=-50`) is 206 with the last 50 bytes;
  6. an unsatisfiable range (`bytes=<size+1000>-`) is 416;
  7. PROPFIND depth-1 returns the directory listing (parse only the
     status line and count of hrefs - the probe does not need full
     XML parsing; a substring count of `<d:response>` or the
     prefixed literal form the webdav 207 uses is enough);
  8. PROPFIND on the file returns its size and an etag property.
  Over TCP (mode `tcp`, plain HTTPS to the webdav frontend):
  9. every response carries `alt-svc: h3="<port>"; persist=1`;
  10. the same Range asserts 4-6 over TCP (parity: the semantics are
      transport-independent).
- Probe output: one PASS/FAIL line per assert, a final tally line
  `h3probe: N/M PASS`, exit non-zero on any failure. The case
  asserts the tally with the lib.sh helpers.

### Contract 6 (docs, you produce this)

- README: a short transport section - mechanism before name (QUIC is
  a UDP-based transport with built-in encryption; HTTP/3 is HTTP
  over QUIC; Alt-Svc is how a client discovers the UDP endpoint and
  falls back to TCP), the config example, and the wifi/distant-link
  rationale in one sentence. Link to the compatibility doc for
  detail. Any README frontend-type enumeration gains `h3`.
- `docs/protocol-compatibility.md`: a webdav Range row
  (implemented; 206/416/multipart; proof: case 37 + the S3 parity
  note that the grammar is shared) and an h3 transport row
  (implemented; same WebDAV semantics over QUIC; degrades to TCP via
  Alt-Svc fallback when UDP is blocked; proof: case 37).
- House copy rules: hyphens not em-dashes; no marketing words;
  mechanism before name. GREP YOUR OWN DIFF for em-dashes before
  reporting - they arrive from the model's default prose style and
  no linter catches them.

## Tasks

### Task 1: the probe

**Objective:** a small Go client that asserts the wire behavior over
both transports.

**Files:**
- Create: `scripts/e2e/h3probe/main.go` (+ `go.mod` if out-of-module)

**Step 1:** Read the repo `go.mod` for the pinned quic-go version.
Build the probe with the same version. Flags: `-url` (base),
`-mode` (`h3`|`tcp`), `-user`, `-pass`, `-port-h3` (for the Alt-Svc
assert in tcp mode), `-bucket`. Content generation: deterministic
bytes (iota-filled buffer) so range asserts are exact.

**Step 2:** Run it against a locally started server (the harness
does this in Task 2; for the unit of this task, a manual run against
`make build` + a hand-written config on a free port is the
verification). All 10 asserts green.

### Task 2: the e2e case

**Objective:** case 37 runs in `make e2e` and passes.

**Files:**
- Create: `scripts/e2e/cases/37-h3-webdav.sh`

**Step 1:** Copy the newest case's skeleton; keep the `BKT=`
convention and create/cleanup pairing. Config: explicit frontends
array with s3, webdav (TCP, ephemeral port), h3 (UDP, ephemeral
port), the harness's generated cert pair. Sequence: start server,
wait for readiness (the harness has a wait helper - find and use
it), run the probe in `h3` mode (asserts 1-8), run it in `tcp`
mode (asserts 9-10), assert both tallies, cleanup (kill the server
- note the harness's bracketed pkill pattern convention if you use
pkill).

**Step 2:** `make e2e` - the full suite green INCLUDING all prior
cases (782+ asserts today; case 37 adds its own tally). Run `make
e2e` ONCE (it takes about 2.5 minutes; do not loop it).

### Task 3: docs

**Objective:** README and the compatibility matrix are honest.

**Files:**
- Modify: `README.md` (transport section + any type enumerations)
- Modify: `docs/protocol-compatibility.md` (the two rows)

**Step 1:** Write the README section per Contract 6; keep it under
30 lines; config example verifiable against the case 37 config shape.

**Step 2:** Add the two compatibility rows; each names its proof
(case 37) and its degrade path. Check the existing row format and
match it exactly.

**Step 3:** Grep your diff for em-dashes and marketing words; fix.

### Task 4: gates

- `go vet ./scripts/e2e/h3probe/` (or the module path you chose)
  green;
- `make e2e` full suite green (run once);
- `gofmt -l` clean on the probe.

## Self-Verification Checklist

- [ ] Probe asserts 1-10 implemented and green against a real server
- [ ] Case 37 follows harness conventions (BKT, create/cleanup,
      lib.sh asserts, readiness wait, bracketed pkill)
- [ ] Config carries explicit s3 + webdav + h3 entries
- [ ] `make e2e` full suite green, prior cases unbroken
- [ ] README section + compatibility rows per Contract 6; 0
      em-dashes in the diff
- [ ] quic-go version in the probe matches the repo pin
- [ ] DO-NOT-TOUCH respected: server code (all of `internal/`),
      `go.mod`/`go.sum` (your probe's own go.mod is fine if
      out-of-module), `scripts/zfs-validate/run-zfs-validation.sh`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Contract 5 assert has a probe line and the case asserts
      the tally
- [ ] The 401 assert proves auth is enforced over QUIC (not just
      over TCP)
- [ ] Range asserts use exact byte comparisons (deterministic
      content), not length-only
- [ ] Alt-Svc assert checks BOTH the header presence and the port
      value
- [ ] Docs state the degrade path (UDP blocked -> TCP) and do not
      overclaim (no "blazing fast" or equivalent)
- [ ] Prior e2e cases all still pass (the harness output says so)

Output: APPROVED or specific gaps with file:line.

## Notes

- **Do not soften asserts into skips.** If an assert fails, the
  failure is a FINDING: investigate before touching the assert. An
  assert that fails because the SERVER is wrong stays in place with
  a FINDING comment only when the fix is out of scope - and then the
  orchestrator decides (repo skill rule from the e2e coverage
  audit).
- **The h3 probe is the first HTTP/3 client this repo ships.** If
  quic-go's client API fights the probe, record the exact friction
  in the report - leaf 04's harness checks reuse the probe, so its
  ergonomics are load-bearing.
- `run-one.sh` in scripts/e2e/ is UNTRACKED sibling WIP - use it if
  helpful for local iteration, never stage it.
