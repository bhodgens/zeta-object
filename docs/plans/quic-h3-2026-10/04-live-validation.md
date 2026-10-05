# Live ZFS Validation (h3 + Range on zfs-meta) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> You run ON the live host path (ssh to zfs-meta is configured). Do NOT
> commit - the orchestrator handles all git operations after review.
> Goal: ALL harness sections green including your new ones, exact tally
> reported, max 3 fix+rerun cycles then report.

## Meta

- **Parent:** ./master.md
- **Scope:** extend `scripts/zfs-validate/run-zfs-validation.sh` with
  an HTTP/3 section and a webdav Range section, then run the full
  harness on zfs-meta until green.
- **Dependencies:** leaf 03 (case 37 + h3probe exist and pass in
  `make e2e`).
- **Estimated Context:** 40K
- **Concurrency Group:** C (live host, solo - two harness runs on the
  host at once corrupt each other)

## Goal

The AGENTS.md ZFS validation rule closes this tree: the freshly built
linux/amd64 binary, deployed to zfs-meta, serves the webdav Range
semantics and the h3 transport against a REAL ZFS dataset with
`events=on`, including the metadata round-trip fetched OVER HTTP/3,
compared against `zfs events -j` ground truth. Dataset destroyed,
server stopped, tally reported.

## Context

- Read the harness first: `scripts/zfs-validate/run-zfs-validation.sh`
  - it builds linux/amd64, deploys the binary plus a fresh cert to
  `zfs-meta:~/zeta-validate/`, recreates `testpool/zval` fresh, runs
  `checks.py` sections, prints a PASS/FAIL table, exits non-zero on
  failure, and destroys the dataset and stops the server.
- Repo-skill harness rules that BIND you:
  - Bracket one char of every pkill pattern (`pkill -f
    'zeta-serve[r]'`): the remote `bash -c` cmdline CONTAINS the
    pattern - an unbracketed pkill kills the ssh channel (exit 255,
    silent).
  - Server runs as `./zeta-server` from the remote dir; kill patterns
    must match the plain cmdline.
  - `--keep-server` semantics: it keeps the stack ONLY on pass; a
    failed run still auto-destroys. To inspect a failure live,
    re-run with the flag and clean up manually after.
  - Every parsed field of a newly probed wire form is suspect until
    one live run confirms it - fixture-pinned unit tests cannot
    catch a wire-format assumption the fixtures share.
  - The probe (`scripts/e2e/h3probe/`) is a GO program: the REMOTE
    host does not need it - run the probe FROM the local machine
    against the remote server's UDP/TCP ports (the harness's local
    python signing block is precedent for local-against-remote
    checks), OR deploy a linux/amd64 probe binary. Pick one, say
    which, and make the checks robust to it (firewall: verify the
    UDP port is actually reachable from outside the host before
    debugging anything else - a UDP firewall drop looks exactly like
    a dead server).
- quic-go version pin: build the probe (if deploying it) with the
  SAME version as the repo's go.mod.

## Interface Contracts (From Parent)

### Contract 6: live validation (you produce this)

- New harness sections in `checks.py` (or the harness's check
  mechanism - follow the existing section structure exactly):
  1. **h3 smoke:** PUT + GET round-trip over HTTP/3 against the
     ZFS-backed bucket (Basic auth, the harness's credentials).
  2. **h3 auth:** unauthenticated GET over h3 is 401.
  3. **h3 Range:** `bytes=0-99` over h3 is 206, bytes match the
     full-body slice exactly.
  4. **Range over TCP (parity):** the same span over the TCP webdav
     listener, byte-identical to the h3 answer.
  5. **metadata round-trip over h3:** `?events` and
     `?events&versions` fetched over HTTP/3, compared against
     `zfs events -j` ground truth the same way the existing TCP
     sections compare them (same comparison code, new transport).
  6. **Alt-Svc:** a TCP response carries `alt-svc: h3="<udp-port>"`.
- The webdav Range semantics themselves were validated in `make
  e2e` (case 37); the LIVE section's job is the transport + ZFS
  interaction, with one Range check as the bridge.

## Tasks

### Task 1: extend the harness

**Files:**
- Modify: `scripts/zfs-validate/run-zfs-validation.sh` (config gains
  the h3 frontend; the server startup gains the UDP port; new check
  section(s))
- Possibly modify: `scripts/e2e/h3probe/main.go` - ONLY if the
  harness needs a flag the e2e case did not (for example
  `-insecure-skip-verify` for the self-signed cert; the e2e harness
  has the same problem - check how existing checks handle the cert
  and follow that). If you must touch the probe, keep the e2e case's
  usage working (same flags, additive only).

**Step 1:** Read the harness; identify the section pattern, the
config template, the startup wait, and the ground-truth comparison.

**Step 2:** Add the section(s) per Contract 6. Bracketed pkill; the
UDP port joins whatever readiness/wait logic the TCP ports use (a
UDP "readiness" probe is just the first h3 request succeeding - do
not invent a UDP ping).

### Task 2: run to green on zfs-meta

**Step 1:** Run `scripts/zfs-validate/run-zfs-validation.sh`. First
run EXPECTS failures - wire forms, firewall, timing. Diagnose each
failure with the repo-skill discipline: fixture tests stay green
while live data proves the assumption wrong -> trust the live data,
fix the check or the server (server bugs become their own report
item - never silently absorb a server fix into a harness commit).

**Step 2:** Iterate to ALL sections green including pre-existing
ones. Max 3 fix+rerun cycles, then report with the exact state.

**Step 3:** Confirm cleanup: dataset destroyed (`zfs list
testpool/zval` fails on the host), server process gone, no stray
UDP listener. The harness does this on exit - verify, do not assume.

### Task 3: report

- The exact PASS/FAIL tally (all sections, not just new ones);
- any server bugs found (file:line, the live evidence, and whether a
  fix commit is proposed);
- which deployment choice you made for the probe (local-run vs
  deployed binary) and the firewall state of the UDP port;
- the quic-go version used.

## Self-Verification Checklist

- [ ] All harness sections green on zfs-meta, exact tally reported
- [ ] Metadata round-trip over h3 compared against `zfs events -j`
      ground truth (not just status-code checks)
- [ ] Dataset destroyed; server stopped; no stray listeners
- [ ] Bracketed pkill patterns throughout the new harness code
- [ ] Any probe change is additive and the e2e case still passes
      (`make e2e` re-run by the orchestrator if the probe changed)
- [ ] DO-NOT-TOUCH respected: everything outside the harness script
      and (additively) the probe

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The tally is a hard number from a real run, not a projection
- [ ] A server-behavior failure, if any, was reported as a finding -
      not fixed by weakening a check
- [ ] Cleanup verified with commands, not assumed
- [ ] The h3 checks would FAIL if the server dropped h3 support
      (negative check: the auth 401 proves the transport actually
      carries requests, and one check exercises a real error path)

Output: APPROVED or specific gaps with file:line.

## Notes

- **UDP firewall is the first suspect** when h3 checks fail from a
  local probe against the remote host: TCP checks passing while h3
  checks time out is the firewall signature, not a server bug.
  Verify with a tcpdump/conntrack one-liner on the host before
  touching server code.
- **Two suites on the host at once corrupt the run** (repo skill:
  the ftp passive-timeout class of false failure). Before starting,
  check for a running harness: `ssh zfs-meta 'pgrep -laf
  "zeta-server|zmetad"'` and wait or coordinate.
- The harness's existing ground-truth comparison code is proven -
  reuse it for the over-h3 fetch; do not write a second comparator.
