# Management API: Live Validation on zfs-meta - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> This leaf runs on a LIVE HOST (zfs-meta). Do NOT commit until the
> orchestrator reviews the harness diff and the full-sweep tally. Explore
> with terminal cat; do not use read_file on existing source files.
> After completing, report the EXACT check tally, every new check name,
> and any harness fixes.

## Meta

- **Parent:** ./master.md
- **Scope:** extend the zfs-validate harness with a management-API
  section and run the full sweep on zfs-meta.
- **Dependencies:** 04, 06 committed. The harness file is the repo's
  highest-collision file: BEFORE starting, run `git status` and
  `git log -3 -- scripts/zfs-validate/run-zfs-validation.sh`; if a
  sibling session owns uncommitted hunks there, STOP and report.
- **Estimated Context:** 40K
- **Concurrency Group:** E (live host)

## Goal

Charter rule 2 (AGENTS.md): prove the management API against real ZFS.
Specifically prove that the API can manage buckets on a real dataset
hierarchy AND that it CANNOT destroy a dataset.

## Context

- `scripts/zfs-validate/run-zfs-validation.sh` builds the server locally,
  deploys it to zfs-meta, recreates a scratch dataset, launches zmetad and
  the server, runs up to 12 numbered sections of checks, prints a
  PASS/FAIL table and exits non-zero on failure. Verify the last section
  number at dispatch time (a sibling may have added one) and use the next.
- The harness already generates a server certificate; it must now also
  generate a CA plus a client certificate, and launch a server phase with
  the admin frontend enabled on a loopback port.
- Facts already established on this host: OpenZFS 2.4.99, only root can
  mount datasets (the harness uses passwordless sudo), and the scratch
  parent is recreated per run.
- pkill discipline: bracket one character in every pattern
  (`zeta-serve[r]`) or the ssh channel kills itself.

## Interface Contracts (From Parent)

Contract 5 is the wire truth. The checks to add:

1. mTLS handshake with the generated client certificate succeeds; no
   certificate is rejected.
2. `GET /status` reports the running frontends.
3. Create a PLAIN bucket through the API (a subdirectory of the scratch
   mountpoint that is not a dataset) and confirm the directory exists.
4. Delete that plain bucket through the API and confirm it is gone.
5. Create a bucket that IS a ZFS dataset (feature enabled in that server
   phase), then `DELETE` it through the API: expect 409 with
   `DatasetBucketNotDeletable` and confirm with `zfs list` that the
   dataset STILL EXISTS.
6. Confirm no management route destroyed anything: after the section,
   the dataset from check 5 is still present, and the section's cleanup
   removes it with `zfs destroy` on the host (not through the API).
7. `GET /config` body contains no secret value from the harness config.

## Tasks

### Task 1: harness section

**Files:** Modify: `scripts/zfs-validate/run-zfs-validation.sh`

**Steps:**
1. Read the harness end to end; find the config and starter sections and
   the last check section.
2. Add the CA and client certificate generation next to the existing
   server certificate generation.
3. Add a server phase (or extend an existing one) with the admin
   frontend on a loopback port and the feature flags you need.
4. Write the checks in the established `check(name, cond, detail)` style,
   with detail strings that surface the wire body on failure.
5. Cleanup in the section's finally path: destroy any dataset the section
   created, remove any plain bucket, kill the extra server phase by
   observed pid.
6. `bash -n` clean before deploying.

### Task 2: the live run

**Steps:**
1. `git fetch --all` in `~/git/zfs-metadata` and check for contract
   movement since the last validation doc; report before adapting if it
   moved.
2. Verify host facts before asserting: the zmetad DB layout version, the
   testpool name, and that the ssh user can run the harness's zfs
   commands through the existing sudo path.
3. Run `./scripts/zfs-validate/run-zfs-validation.sh`. Max 3 fix+rerun
   cycles on harness bugs. Fixing the harness is expected; a SERVER bug
   is a finding - report it verbatim, do not patch server code (a
   one-line obvious fix may be reported as a separate commit candidate).
4. Record the exact final tally and every new check name in
   `docs/validation-management-api-<YYYY-MM-DD>.md`, in the style of
   `docs/validation-zfs-bucket-datasets-2026-10-03.md` (run metadata,
   host facts, check list, findings). Report the actual filename used so
   the docs cross-links can be updated.

## Self-Verification Checklist

- [ ] New checks ALL green AND every pre-existing check still green; exact tally recorded
- [ ] The dataset from the refusal check survives the API delete (verified with `zfs list` in the check AND after the run)
- [ ] Post-run host state clean: no extra server processes, no leftover datasets or snapshots, no leftover client certificate files
- [ ] Validation doc written in the established format
- [ ] pkill patterns bracketed; no ssh-channel suicides (exit 255)
- [ ] `git diff --cached` contains ONLY the harness and the validation doc

**DO NOT COMMIT** until orchestrator review.

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The refusal check proves the dataset survived, with the actual `zfs list` output captured
- [ ] The tally in the report matches the run log (spot-check the log tail)
- [ ] Cleanup cannot wedge the shared testpool for sibling sessions
- [ ] The validation doc cites real observed output, not restated design

Output: APPROVED or specific gaps.

## Notes

- The most valuable check in this section is number 5: it is the only
  place where the "the API cannot destroy a dataset" decision is proven
  against real ZFS. If it cannot be made to run, say so loudly rather
  than marking the leaf complete.
- `ZETAOBJECT_ASSUME_ZFS` is irrelevant here (real ZFS host).
