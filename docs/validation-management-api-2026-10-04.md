# Live Validation: Management API on zfs-meta (2026-10-04)

Leaf 07 of the `management-api-2026-10` tree. Repo HEAD `456d84e` (leaf 06
committed). Harness: `scripts/zfs-validate/run-zfs-validation.sh` extended with
**section 13** (management API over mTLS) behind **two new admin server
phases**.

## Run result

| Sweep | Tally |
|---|---|
| Sections 0–11 (pre-existing S3 + events) | **90/90 PASS** |
| Section 12 (`zfs_bucket_datasets`) | **33/33 PASS** |
| Section 13 (`plain` admin phase: mTLS, /status, /config, plain bucket CRUD) | **10/10 PASS** |
| Section 13 (`ds` admin phase: dataset bucket + refusal) | **7/7 PASS** |
| Run total | **140/140 PASS — exit 0** |

All new checks green and every pre-existing check still green, first run (zero
harness fix+rerun cycles). The only tally movement versus the 2026-10-03 doc is
sections 0–11 at 90 (was 91): the `s11 pool bcloneratio` check degraded to an
**ADVISORY** this run (the pool's lazy BRT accounting had not re-converged after
import within the 30s window — the committed `ad4c0b5` behavior; the
authoritative clone evidence, the allocated-byte delta check, passed and is
counted). Log line:

```
ADVISORY: pool bcloneratio stayed 1.0 within 30s of the clone (lazy accounting
on a recently-imported pool); the allocated-byte delta check above is the
authoritative clone evidence
```

## Host facts (verified before asserting)

- Host: zfs-meta, Ubuntu 24.04.2 LTS (`whoami` = `caimlas`), OpenZFS
  **2.4.99-1**. `zpool list` shows `testpool` (already existed).
- `curl` at `/usr/bin/curl`, `openssl` at `/usr/bin/openssl` (both required:
  the admin mTLS checks shell `curl` on the host, since the admin listener
  binds loopback and is unreachable from the workstation).
- Passwordless sudo available (`sudo -n true` ok); `/dev/zfs` is `0666`.
- `testpool/zval` was **absent** before the run (fresh state) and recreated by
  the harness fresh each run.
- Installed zmetad DB at `~/zeta-validate/zmetad.db`; `db_schema_version`
  accepted in the 5..8 range with `events_schema_version` consistent (section 0
  green).

## Upstream contract check (zfs-metadata)

`git fetch --all` in `~/git/zfs-metadata`; `origin/extended-metadata` HEAD is
still **`19c157b7f`** ("tests: ZFS_EV_PRINCIPAL coverage and schema 3/8 pins"),
the same head the 2026-10-03 validation doc recorded. **No contract movement**
— the wire/layout contract the server consumes (layout 8 / wire 3) is
unchanged, so no adaptation was needed.

## Section 13 design

The admin frontend is a dedicated mTLS JSON surface on its OWN loopback
listener. Two server phases (both `"type": "admin"`, `clientCAFile` pointing at
the generated CA), because the create path is **unconditionally
dataset-backed when the feature is on** (`internal/bucketmanager` `Env.create`:
`if e.Provisioner != nil { return e.createDataset }`) — a plain-directory
bucket therefore requires the feature OFF:

- **plain phase** — `zfs_bucket_datasets: false`; S3 `listenAddr :9709`, admin
  `127.0.0.1:9710`; runs unprivileged. Covers mTLS, `/status`, `/config`, and
  plain-bucket create/delete.
- **ds phase** — `zfs_bucket_datasets: true`, `dataDir /testpool/zval/` (parent
  = the scratch dataset); S3 `listenAddr :9711`, admin `127.0.0.1:9712`; runs
  under `sudo -n -E` (only root can mount children on this host, F-zbd-3).
  Covers the dataset bucket and the DELETE refusal.

Ports 9709–9712 are distinct from every other listener in the harness
(`:9707` phase 1, `:9708` section 12). The harness generates a trusted CA plus
a pinned-CN client certificate (`CN=zval-admin`) and a second same-CN
certificate from a **different** CA, deployed alongside the existing server
cert. Requests run `curl` ON the host (authenticated with the client
certificate). Every `pkill` pattern is one-char bracketed
(`'[.]/zeta-serve[r]'`); the extra phases are killed by observed pid
(`mgmt-<mode>.pid`) plus the sudo bracketed pkill, and the section's
finally-path destroys any dataset it created with `zfs destroy` on the host
(never through the API).

## Section 13 check list (exact names, 17/17 PASS)

plain phase (10):

1. s13 mTLS: valid client certificate -> 200 on /status
2. s13 mTLS: request with NO certificate rejected (handshake failure or 401)
3. s13 mTLS: certificate signed by a DIFFERENT CA rejected
4. s13 /status reports running frontends including admin
5. s13 POST /buckets (plain) -> 200 (bucket created)
6. s13 plain bucket directory exists on the scratch mountpoint
7. s13 DELETE plain bucket -> 200 (bucket deleted)
8. s13 plain bucket directory gone after the API delete
9. s13 GET /config -> 200
10. s13 /config body contains no harness secret value (masked)

ds phase (7):

11. s13 POST /buckets (dataset feature on) -> 200
12. s13 bucket is a real ZFS dataset under the scratch parent
13. s13 DELETE dataset-backed bucket -> 409 (refused)
14. s13 409 body carries code DatasetBucketNotDeletable
15. s13 dataset STILL exists after the API delete refusal (zfs list proof)
16. s13 dataset still present at section end (the API destroyed nothing)
17. s13 dataset destroyed on the HOST via zfs destroy (cleanup, not the API)

## Refusal check evidence (the load-bearing check)

Verbatim capture from a focused manual probe of the same deployed binary and
`config-mgmt-ds.json` on the live host (the harness check 15 captures the same
`zfs list` output in its detail string; this is the captured proof):

```
### POST /buckets {"name":"mgmt-ds"}
{"created":true,"name":"mgmt-ds"}
HTTP 200

### zfs list BEFORE delete
testpool/zval/mgmt-ds

### DELETE /buckets/mgmt-ds
{"error":{"code":"DatasetBucketNotDeletable","message":"bucket \"mgmt-ds\" is a ZFS dataset (testpool/zval/mgmt-ds); the management API does not destroy datasets. Remove it on the host with `zfs destroy testpool/zval/mgmt-ds`."}}
HTTP 409

### zfs list AFTER the refused API delete (dataset must STILL exist)
testpool/zval
testpool/zval/mgmt-ds

### snapshots under the parent
(no snapshots)
```

The dataset exists before the delete, the API answers **409
`DatasetBucketNotDeletable`**, and the dataset **still exists afterwards** —
user decision 5 is proven against real ZFS: no management API path destroys a
ZFS dataset. The host-side cleanup then removes it with `zfs destroy` (check
17 confirms it is gone and that the API did not do it).

## Findings

**None (no server bugs).** Every management-API behavior under test matched the
committed contract on the live host: mTLS handshake with the generated client
certificate succeeds; no certificate and a wrong-CA certificate are both
rejected at the TLS handshake (no detail leaked); `/status` lists the `admin`
frontend; `/config` masks the configured `secretKey` values; a plain bucket is
created as a directory and deleted; a dataset-backed bucket is provisioned as a
real child dataset and its DELETE is refused with 409 without destroying it.
Zero server-side defects observed.

## Cleanup state (post-run, verified)

`pgrep -af 'zeta-serve[r]|zmeta[d]'` → no matches. `zfs list -H -o name -r
testpool` → `testpool`, `testpool/fs1` only (the pre-existing sibling dataset;
`testpool/zval` and all its children destroyed). `zfs list -t snapshot -H -o
name -r testpool` → no snapshots. No certificate files outside the harness's
`~/zeta-validate/` deploy directory (`ls /tmp/*.pem` → none). The harness
deploys its TLS material (server cert, CA, client cert, wrong-CA cert) into
`~/zeta-validate/` by convention and overwrites it every run, exactly as it
already did for `cert.pem`/`key.pem` before this leaf; that directory is the
harness's working state, not a cleanup leak.

## Deviations from spec

1. **Two admin phases instead of one.** The leaf/task text describes a single
   admin phase that enables `zfs_bucket_datasets` "so a bucket can be created
   as a real dataset", while also requiring a PLAIN bucket created through the
   API. Those are mutually exclusive in one phase: `internal/bucketmanager`
   `Env.create` calls the dataset provisioner unconditionally whenever the
   feature is on, so a plain bucket is only reachable with the feature OFF.
   Implemented as two admin phases (plain = feature off, ds = feature on),
   both `type admin` on distinct loopback ports. All required checks are
   present and green.
2. **Extra check (wrong-CA rejection).** Added "certificate signed by a
   DIFFERENT CA rejected" beyond the required valid/no-cert pair; it is a real
   security property (issuer, not just CN, is enforced) and passed.
3. **`curl`-on-host transport.** The admin listener binds loopback by design,
   so the section's requests run `curl` on the host rather than from the
   workstation; the client certificate is deployed to the harness's existing
   `~/zeta-validate/` directory.
