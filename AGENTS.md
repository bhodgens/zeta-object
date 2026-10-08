# AGENTS.md

Guidance for AI agents and humans working in this repository.

## Project charter: dumb gateway/proxy

zeta-object is a **dumb gateway/proxy in front of block storage**, as much as
possible. The backing filesystem is the source of truth; the server adds
protocol translation, not a storage layer of its own.

**No persistent server-owned state with identity association.** The server must
not store anything persistent that associates data with zeta-object itself -
no database, no server-owned index, no sidecar that outlives the objects it
describes - except:

- the per-directory `.metadata/` sidecars, and ONLY when the backing storage
  is not ZFS (ZFS-backed buckets get their history from the dataset event log
  instead - never duplicate that into sidecars);
- transient upload staging under the bucket's own `.metadata/.uploads/`,
  cleaned by the expiry sweeper;
- the append-only per-request audit log (charter exception, decided
  2026-10-02): an append-only record of authenticated requests (principal,
  operation, object, timestamp) written by the gateway for forensic
  attribution. Allowed ONLY as write-once at the auth boundary - the data
  path and enforcement never read it, so the gateway never acts on stored
  audit state and the backing filesystem remains the sole source of truth
  for data. It must live on a ZFS dataset (or the bucket's own storage) and
  be protected by ZFS snapshot plus scheduled off-box `zfs send`. Treating
  the audit log as queryable server state, or letting any request path
  consult it, violates this charter and requires a new decision.

Any feature that wants persistent per-object or per-bucket bookkeeping must
either (a) derive it from the backing filesystem (the MetadataProvider seam),
(b) store it in the object's own sidecar file, or (c) not persist it. If a
design needs a server-owned store, stop and bring the design back for a
decision instead of implementing the store.

## ZFS validation rule (hard requirement)

The project's differentiating capability is ZFS-event metadata. Every change
that touches the data plane, metadata surface, or backends MUST be validated
against real ZFS on the `zfs-meta` host (ssh access configured) before it is
considered done - unit tests and fixtures alone do not close a change. The
host runs OpenZFS with the extended-metadata branch (`events` +
`events_size` dataset properties). Deploy the freshly built linux/amd64
binary, create a scratch dataset with `events=on`, run the change's own e2e
case plus the metadata round-trip (`?events`, `?events&versions`), and
confirm the output matches `zfs events -j` ground truth. Destroy the scratch
dataset afterwards.

**Tooling:** `scripts/zfs-validate/run-zfs-validation.sh` automates all of the
above — run `scripts/zfs-validate/run-zfs-validation.sh` after building (it
builds the linux/amd64 binary itself, deploys it plus a fresh cert to
`zfs-meta:~/zeta-validate/`, recreates `testpool/zval` fresh, runs the S3 +
`?events`/`?events&versions` checks against `zfs events -j` ground truth,
prints a PASS/FAIL table, exits non-zero on failure, and destroys the dataset
and stops the server). Add change-specific probes by extending the embedded
`checks.py`; `--keep-server` leaves the stack up for manual poking.

## E2E coverage rule (hard requirement)

Every new user-facing feature MUST ship with e2e or wire-level test coverage in
`scripts/e2e/` in the SAME change. "User-facing" means any of:

- API surface - new S3 operations, query parameters, headers, or response fields
- Config surface - new `config.json` keys, env vars, or backend options
- Backend capability - new behavior in `internal/backend/*` visible to clients
- Frontend capability - new behavior in `internal/frontend/*` visible to clients

A feature merged without a case under `scripts/e2e/cases/` is incomplete.

## Coverage floors move with code

When a change moves code between packages (file split, package extraction), the
coverage floors MUST be re-measured and updated in the SAME change. Set each
floor to the measured value rounded down, with a comment saying when to ratchet
it back up as tests land.

Incident: the frontend-interface split (2026-09) moved code out of package main
and dropped aggregate coverage from above the old floor to 47.9% while
`COVER_MIN` still said 70 - the gate was red for everyone and got ignored. Do
not let floors drift from reality again. See the AGENTS.md rule in the Makefile
and `.github/workflows/check.yml` comments.

## E2E harness

Run the whole suite with `make e2e` (wraps `scripts/e2e/run-e2e.sh`). The
harness builds the server, generates temp certs and a temp dataDir, starts on a
free port, and runs every `scripts/e2e/cases/*.sh` in lexical order. To add a
case: copy an existing numbered case, keep the `BKT=` bucket convention and the
create/cleanup pairing, and use the assert helpers from `scripts/e2e/lib.sh`.
Cases also exist for interop clients (`boto3`, `mc`, `rclone`, `owncloudcmd`) -
extend those when the feature is client-visible there too.

### Cleanup is a harness obligation (bughunt 2026-10-08)

A case MUST leave the machine as it found it. Concretely:

1. Every process the case starts gets an `EXIT` trap that stops it, and every
   temp path the case creates - including any `mktemp /tmp/e2eNN-*` FILE, not
   just the temp ROOT dir - is named in that trap's `rm`. A per-request body
   file outside the root is the shape that leaks: cases 32/33 did it and left
   450 files in `/tmp` before the audit caught it.
2. To debug ONE case, use `scripts/e2e/run-one.sh <case-name-without-.sh>`,
   not the full suite. It builds the server if missing, runs the case against
   its own port and temp dataDir, prints the server log, and keeps the temp dir
   on failure (or with `ZETAONE_KEEP=1`).
3. `scripts/e2e/e2e-cleanup.sh` holds the shared contract: `e2e_sweep_stale`
   removes stale `/tmp/e2e[0-9]*`, `/tmp/zetaobject-e2e.*` and `/tmp/zeta-one.*`
   state plus orphaned `zeta-object-server` processes, and `e2e_reap_pid` is the
   one TERM-then-KILL stop used by every cleanup. Both `run-e2e.sh` and
   `make e2e-clean` source that file - do not re-implement the logic.
4. Run `make e2e-clean` after a run that was interrupted, or whenever you are
   unsure. `E2E_SWEEP_MIN_AGE` (seconds, default 30) is the safety cutoff: the
   sweeper never touches younger state and never kills a server that is still
   LISTENING, so a concurrent `make e2e` is safe. Pass `E2E_SWEEP_MIN_AGE=0`
   only when you know nothing else is running.
5. `run-e2e.sh` traps INT and TERM as well as EXIT, so Ctrl-C still reaps the
   server and sweeps.

## Plan trees

Multi-leaf work lives under `docs/plans/<tree>/master.md` (for example
`docs/plans/hardening-2026-09/master.md`). Each leaf is one file; a leaf is done
only when its code, tests, and docs land together. Start new campaign plans
there, not in ad-hoc notes.
