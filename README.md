# zeta-object

**A single-binary S3 server with filesystem superpowers.**

zeta-object speaks the S3 protocol and stores your data where you can see it: plain files, in your directories, on your filesystems. Where every other object server buries your data in its own opaque store, zeta-object treats your filesystem as the source of truth - and adds capabilities no cloud S3 can offer, because only a filesystem co-located with your data can offer them.

- **One static binary.** No database, no etcd, no external services. `make build`, run it, done.
- **Zero-format storage.** Objects are plain files; metadata is a JSON sidecar. Your data is readable with `cat` and `ls` with the server stopped. Point a bucket at `/var/log`, a ZFS dataset, an NFS mount, or a directory of symlinks and it is an S3 bucket *now*.
- **Standard, verified wire compatibility.** AWS CLI, boto3, and mc work against it - proven by an interop e2e suite and a ceph/s3-tests ratchet, not by marketing.
- **Protocol-flexible by design.** A pluggable frontend/backend architecture (S3 today; WebDAV, FTP/SFTP, ownCloud tracked) over a neutral object model - one implementation per protocol and per storage, not one per combination.
- **Extensible metadata.** A probe-based MetadataProvider seam attaches enrichment capabilities to buckets when - and only when - the underlying filesystem supports them. The first provider reads ZFS per-dataset file-event logs, giving per-object history and version-style listings that hosted S3 cannot give you.

## The pitch: what proves zeta-object different

Most "S3-compatible" servers are the same idea restated: a service that owns a black-box store and translates S3 verbs against it. zeta-object takes the opposite bet, and these five properties follow from it:

1. **Your data is never held hostage.** Every object is a file you own, at a path you chose. Back up with rsync, replicate with ZFS send, grep it, migrate away by copying a directory. There is no export step because there is nothing to export.
2. **Existing directories become S3 buckets with zero migration.** Bucket `logs` at `/var/log` means the decade of log files already on disk is immediately listable, downloadable, and presign-able over S3 - byte-for-byte, no import, no copy. Symlinks are followed, so a bucket can live anywhere.
3. **Filesystem capabilities become S3 capabilities.** When a bucket sits on a ZFS dataset with the `org.openzfs:events` feature (per-dataset file-op history), zeta-object detects it at startup and serves `GET /<bucket>?events` and `GET /<bucket>?versions` derived from the kernel's own record of what happened to each file - create, rename, truncate, delete - with loss indicators. No hosted S3 offers object history; no opaque object server can borrow it from the filesystem. When the filesystem does not support it, the capability is simply absent (a clean 503), never faked.
4. **Pluggable on both axes, honest about semantics.** Frontends (client protocols) and backends (storage) plug into one neutral object model, and the seams reject what a protocol cannot express instead of silently emulating it. A parity gate proves an enabled metadata provider changes nothing about core S3 responses.
5. **Small enough to read, hardened enough to trust.** One Go binary, stdlib-only dependencies, and a gate wall: 200+ unit tests, 213-assert e2e suite, race detector, fuzzing, ceph/s3-tests conformance ratchet, staticcheck/gosec, and a pre-commit chain that enforces all of it. The codebase is small enough that an afternoon of reading covers every line that touches your data.

## Who it is for

- **Homelab and self-hosting:** S3 for your existing NAS directories, ZFS pools, and backup trees - with snapshots and event history baked in.
- **Legacy storage gateways:** give an old file server or archive an S3 API without moving a byte.
- **Development and CI:** a dependency-free S3 endpoint with real SigV4, range requests, multipart, and presigned URLs - honest errors, fast startup, no containers required.
- **Backup targets:** rclone and any S3-native backup tool, pointed at directories you control and replicate on your own terms.

## Feature highlights

**S3 core** - buckets, objects, multipart (parts 1-10000, expiry sweeper), ListObjectsV2 (prefix/delimiter/continuation/`encoding-type=url`), CopyObject (COPY/REPLACE directives), batch DeleteObjects, `?versions` listing, Range requests (206/416), conditional GET (If-Match/If-None-Match/If-(Un)Modified-Since), presigned URLs, verified `aws-chunked` streaming signatures.

**Auth** - AWS Signature Version 4 (header and presigned), 15-minute clock-skew window, region pinning (`us-east-1`).

**Event Actions** - run shell commands on upload/download/delete with glob matching, per-subdirectory merge/override/disable inheritance, inactivity triggers (e.g. `zfs snapshot` after 30 quiet minutes), safe single-quote shell-quoting of all variables, timeouts with process-group kill. See [Event Actions](#event-actions).

**Metadata capability endpoints** - when a bucket's filesystem provides an event log (ZFS `org.openzfs:events`): object and bucket event history as JSON, a versions-style XML listing derived from the log (delete markers included, `IsLossy`/`RecordsLost` flags when the ring buffer wrapped), probed lazily per bucket. Absent capability = clean 503, zero overhead.

**Operations** - HTTPS-only (TLS 1.2 minimum), graceful 30s shutdown drain, per-key write serialization, atomic writes (temp + fsync + rename) so a crash never truncates an object, path-traversal rejection, optional `max_put_bytes` per backend (default 5 GiB, the S3 single-PUT limit).

**Gateway architecture** - `backends` config selects storage per bucket (filesystem today; the seam carries a 17-subtest conformance suite every backend must pass); `frontends` config selects client protocols with optional dedicated TLS listeners; unknown backend/frontend types fail startup loudly, never silently.

## Quickstart

```bash
make hooks        # one-time: install git hooks (pre-commit quality gates)
make build        # compile ./zeta-object-server
make certs        # generate self-signed certs/ (SAN: localhost, 127.0.0.1) if missing
make run          # start the server (HTTPS on :8443)
```

Then, from another terminal:

```bash
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
aws s3 ls --endpoint-url https://localhost:8443 --no-verify-ssl --region us-east-1
```

Quality gates and tests:

```bash
make test         # unit tests with coverage summary
make check        # full local gate: build, vet, fmt, lint, tests, race, vuln, secrets
make e2e          # end-to-end suite: 213 asserts over 17 cases (incl. boto3 + mc interop)
```

## Credentials Configuration

The server uses **AWS Signature Version 4** for authentication. Configure matching credentials on both the server and client.

The server has a **multi-identity registry**: the environment credential pair always exists as the wildcard-grant `env` identity (backward compatible with older single-pair deployments), and any number of additional identities with per-bucket grants can be declared in `config.json` under `identities`. Clients authenticate with their own access key pair and receive only the grants configured for them.

### Server-Side Credentials

| Environment Variable | Default Value | Description |
|---------------------|---------------|-------------|
| `ZETAOBJECT_ACCESS_KEY` | `minioadmin`  | Access Key ID |
| `ZETAOBJECT_SECRET_KEY` | `minioadmin`  | Secret Access Key |

Setting either variable to an empty string logs a warning and falls back to the default - it does not disable default credentials.

```bash
export ZETAOBJECT_ACCESS_KEY="myaccesskey"
export ZETAOBJECT_SECRET_KEY="mysecretkey"
./zeta-object-server
```

### Multiple identities and per-bucket grants

Deployments that need more than one client key declare additional identities in `config.json`:

```jsonc
{
  "identities": [
    {
      "name": "ci-bot",                    // required, unique, log-safe
      "accessKey": "AKIDZETACIBOT01",      // required, unique across identities AND the env pair
      "secretKey": "…",                    // required
      "grants": {                          // optional; absent ⇒ full read/write everywhere
        "*": "readwrite",                  // wildcard bucket allowed
        "photos": "readonly"               // "readonly" | "readwrite" (write implies read)
      },
      "sshPublicKeys": []                  // optional; reserved for the future SFTP frontend
    }
  ],
  "auth": { "mode": "" }                   // "" (default, auth required) | "none" (see below)
}
```

Semantics:

- The env pair (`ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`, default `minioadmin`) is **always** present as identity `env` with full read/write access to every bucket — configs without an `identities` key behave exactly as before this feature existed.
- A `readonly` grant permits GET/HEAD/list operations; a `readwrite` grant (or the wildcard) permits writes too. Denied operations return the standard S3 `AccessDenied` (403) error.
- An access key that is not in the registry is rejected with `InvalidAccessKeyId`, unchanged.
- Validation is fail-loud: a duplicate access key (between identities, or against the env pair) or any invalid identity aborts startup — never a silent fallback.

### Zero-auth dev mode (opt-in, loud)

```jsonc
{ "auth": { "mode": "none" } }
```

`auth.mode: "none"` disables authentication entirely: every request is accepted as an anonymous principal with full read/write access. It is designed for local development only and is loud by design — the startup banner reads `WARNING: AUTHENTICATION DISABLED …`, and every unauthenticated request logs its own `WARNING` line. It is never the default.

### Client-Side Configuration (AWS CLI)

```bash
aws configure --profile zetaobject
```

Enter `minioadmin`/`minioadmin` (or your custom pair). The region **must** be `us-east-1`: the server is pinned to that region and rejects other regions with `AuthorizationHeaderMalformed`.

```bash
aws s3 ls --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
```

## Configuration

zeta-object uses a JSON configuration file (see `config.json.example` for a commented sample). Default path: `config.json` in the current directory; override with `ZETAOBJECT_CONFIG`. Unknown keys and malformed values fail startup loudly - a typo can never silently disable a setting.

### config.json keys

| Key | Default | Description |
|-----|---------|-------------|
| `dataDir` | `./data/` | Root directory for auto-discovered buckets. Any subdirectory (including symlinks) becomes a bucket. |
| `listenAddr` | `:8443` | HTTPS listen address (host:port). |
| `certFile` | `certs/cert.pem` | TLS certificate path. |
| `keyFile` | `certs/key.pem` | TLS private key path. |
| `buckets` | `{}` | Map of bucket names to custom filesystem paths (string form, or `{"path": ..., "backend": ...}` object form). |
| `backends` | - | Optional backend registry: backend type name to construction config. Unknown types abort startup. |
| `frontends` | - | Optional frontend list (default: one S3 frontend on `listenAddr`). Each entry may set its own `listenAddr` for a dedicated TLS listener. |
| `identities` | `[]` | Optional additional auth identities (name, accessKey, secretKey, optional per-bucket `grants`, optional `sshPublicKeys`). See [Multiple identities](#multiple-identities-and-per-bucket-grants). |
| `auth` | - | Optional auth settings; `auth.mode: "none"` enables the loud zero-auth dev mode. |

Credentials are **not** set in the config file - environment variables only.

### Environment variables

| Variable | Description | Default |
|----------|-------------|---------|
| `ZETAOBJECT_CONFIG` | Path to configuration file | `config.json` |
| `ZETAOBJECT_LISTEN_ADDR` | Overrides `listenAddr` for the default frontend (env beats config file) | - |
| `ZETAOBJECT_ACCESS_KEY` | Access Key ID for authentication | `minioadmin` |
| `ZETAOBJECT_SECRET_KEY` | Secret Access Key for authentication | `minioadmin` |

### Bucket discovery and custom buckets

1. **Custom buckets** (from the `buckets` map) load first; **auto-discovered buckets** come from scanning `dataDir`
2. Symlinks are followed; custom buckets win name collisions
3. Custom buckets **cannot be created or deleted** via the S3 API (protected), and can point at any readable directory

Example - expose `/var/log` as bucket `logs` and `/home/user/documents` as `docs`:

```json
{
  "dataDir": "./data/",
  "buckets": {
    "logs": "/var/log",
    "docs": "/home/user/documents"
  }
}
```

```bash
aws s3 ls s3://logs/ --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
aws s3 cp s3://docs/report.pdf ./report.pdf --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
```

### On-disk layout

```
<dataDir>/              # or a custom bucket path
  <bucket>/
    <objects>           # object data: plain files, byte-for-byte
    !data/              # shadow dir for keys whose layout would collide with metadata (percent-encoded)
    .metadata/
      <object>.meta     # JSON sidecar per object
```

Data files are plain bytes. The `.metadata/` sidecars carry content type, ETag, size, timestamps, and `x-amz-meta-*` user metadata. A key that would collide with sidecars (e.g. `.metadata` itself) lives percent-encoded under `!data/`.

## Using with S3 Clients

*   **Endpoint URL**: `https://localhost:8443` (or your `listenAddr`).
*   **Credentials**: `minioadmin`/`minioadmin` (default) or your custom pair.
*   **Region**: `us-east-1` - other regions are rejected.
*   **SSL**: the bundled self-signed cert covers `localhost`/`127.0.0.1`; otherwise `--no-verify-ssl`.

```bash
aws s3 mb s3://mytestbucket --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
aws s3 cp test.txt s3://mytestbucket/test.txt --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
aws s3 ls s3://mytestbucket --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
aws s3 cp s3://mytestbucket/test.txt downloaded_test.txt --profile zetaobject --endpoint-url https://localhost:8443 --no-verify-ssl
```

Presigned URLs and `mc`/`rclone` work the same way - see the interop e2e cases (`scripts/e2e/cases/12-interop-boto3.sh`, `13-interop-mc.sh`) for working examples.

## Metadata Capability Endpoints (ZFS events)

If a bucket's backing dataset is ZFS with the `org.openzfs:events` pool feature enabled (file-level operation history per dataset), zeta-object detects it and exposes the log over S3-style subresources:

- `GET /<bucket>/<key>?events` - the key's event history as JSON (op, txg, old/new name for renames, sizes for truncates)
- `GET /<bucket>?events` - bucket-level recent history
- `GET /<bucket>?events&versions` - a versions-style XML listing derived from the log: newest-first, per-key `IsLatest`, delete markers for removes, `IsLossy`/`RecordsLost` if the ring buffer wrapped

The capability is probed per bucket (filesystem type + feature check). Non-ZFS buckets get a clean `503 NotImplemented`; nothing is emulated. `uid`/`gid` are not exposed. Requires a host with the `zfs` binary in `PATH`.

## Event Actions

Configurable shell commands fire in response to S3 operations, via `.bucket-actions` files (JSON5) placed in bucket directories. Subdirectories inherit and can merge, override, or disable parent actions.

Use cases: post-upload processing (thumbnails, transcoding), delete-after-download cleanup, inactivity snapshots (`zfs snapshot` after 30 quiet minutes), audit logging.

```json5
{
  "version": "1.0",
  "after_upload": [
    {
      "name": "thumbnail-generator",
      "patterns": ["*.jpg", "*.png"],
      "command": "/scripts/thumb.sh $FILE_PATH",
      "async": true,
      "timeout": 60
    }
  ],
  "inactivity_timeout": {
    "duration": "30m",
    "command": "zfs snapshot tank/data@$(date +%s)",
    "reset_on": ["upload", "delete"]
  }
}
```

Variables: `$FILE_PATH`, `$METADATA_PATH`, `$BUCKET_NAME`, `$BUCKET_PATH`, `$OBJECT_KEY`, `$CONTENT_TYPE`, `$ETAG`, `$SIZE`.

Security properties: every substituted value is POSIX single-quote escaped (object keys can never inject commands) - reference variables unquoted (`/script.sh $FILE_PATH`); 30s default timeout with process-group kill; output captured per stream, truncated at 1MB; async by default (`"async": false` blocks the response); keys with `..`/`.metadata` segments are rejected, so actions never touch files outside the bucket tree.

Inspect configured actions with `./scripts/show-bucket-actions.sh data/`.

## Protocol Conformance

zeta-object is baselined against the industry-standard [ceph/s3-tests](https://github.com/ceph/s3-tests) suite. `make conformance` builds the server, launches it on a free HTTPS port, runs the in-scope pytest subset (277 tests - buckets, objects, listing, multipart, copy, conditional, range, presigned), and exits non-zero only when a previously-passing test regresses against the committed ratchet `scripts/conformance/baseline.txt`. The full matrix with per-failure triage: [docs/conformance/2026-09-28-matrix.md](docs/conformance/2026-09-28-matrix.md).

The e2e suite (`make e2e`, 213 asserts / 17 cases) additionally covers every user-facing surface - including custom buckets, backend/frontend configuration, the metadata endpoints, and live boto3/mc interop - per the repo rule in [AGENTS.md](AGENTS.md).

## Architecture

```
FRONTEND (client protocols)          BACKEND (storage)
  S3 today; WebDAV/FTP/ownCloud        filesystem today; crush-lite ZFS
  tracked in issues #1-#3              ring + S3 upstreams planned
          \                               /
           \                             /
        neutral object model (internal/objectmodel)
                     |
        optional MetadataProvider seam (internal/metadata)
        ZFS events provider: history + derived versions
```

Each axis plugs in independently; one implementation per protocol and per storage, not one per combination. The neutral model's canonical metadata surface is enforced by a parity gate (`make parity-test`): an attached provider can add capability endpoints but can never change core S3 responses.

```
internal/
├── objectmodel/   # neutral types, canonical S3 metadata surface, parity gate
├── backend/       # Backend interface + conformance suite every backend must pass
│   └── fsbackend/ # filesystem storage (flat + shadow layout, atomic writes, locks)
├── frontend/      # Frontend interface, registry, conformance suite
│   ├── s3/        # the S3 HTTP frontend: SigV4, dispatch, handlers, XML
├── metadata/      # MetadataProvider seam, registry, ZFS events consumer
└── auth/          # Authenticator/Identity/Grant v1 (per-frontend adapters)
```

## Building and Running

```bash
make build    # compile ./zeta-object-server
make run      # build + start (HTTPS on :8443)
make clean    # remove build artifacts
```

`make certs` generates self-signed certificates with SANs for `localhost`/`127.0.0.1` (only if missing). SIGINT/SIGTERM trigger graceful shutdown, draining in-flight requests up to 30 seconds.

## Testing and Quality Gates

```bash
make test              # unit tests with coverage summary
make check             # build, vet, fmt, lint, tests, race, vuln, secrets
make e2e               # 213-assert end-to-end suite, 17 cases
make parity-test       # metadata-provider parity gate (FS vs provider-backed identical)
make test-cover-enforce # aggregate coverage floor (ratchets up over time)
make conformance       # ceph/s3-tests subset vs committed ratchet
make fuzz              # fuzz targets
```

Every pull goes through the pre-commit chain (secrets scan, vet, error-pattern check, gosec, staticcheck) and CI runs per-package checks with per-package coverage floors.

## Known Limitations

*   No ACLs or bucket policies; authorization is per-identity bucket grants (`identities` config, GH issue #4).
*   Region pinned to `us-east-1`.
*   Object keys with `..` or `.metadata` path segments are rejected, and keys must be in canonical form (safety over S3 compatibility; no `a//b` aliasing).
*   S3 versioning is not implemented; `?versions` lists existing objects, and the ZFS-events-derived version listing is an extension, not S3 versioning.
*   Multi-range GET is unsupported (single range: 206/416).

## Roadmap

*   **Frontend protocols**: WebDAV (#1), FTP/FTPS + SFTP (#2), ownCloud (#3) - the pluggable seam and conformance suite are in place.
*   **Pluggable authentication**: multi-identity keys with per-bucket grants across all frontends (#4) — S3 core is DONE (registry + grants + dev mode); Basic-auth/SFTP frontends land with their protocol issues.
*   **More backends**: crush-lite distributed ZFS ring ([plan](docs/plan-distributed-zfs-backing.md)), S3-compatible upstreams.
*   **More metadata providers**: NTFS USN journal, NILFS2 - the seam probes rather than assumes.

See [docs/plans/](docs/plans/) for the plan trees and [docs/gap-closure.md](docs/gap-closure.md) for development history.
