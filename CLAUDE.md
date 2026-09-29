# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Run Commands

```bash
make hooks        # Install git hooks (core.hooksPath -> .githooks); run once per clone
make build        # Compile the Go server to ./mini-s3-server
make certs        # Generate self-signed certs in certs/ (SAN: localhost, 127.0.0.1) if missing
make run          # Build and start the server (HTTPS on :8443; run 'make certs' first)
make clean        # Remove the compiled binary and coverage artifacts
```

### Test and quality gates

```bash
make test                # go test -count=1 with coverage summary
make test-race           # tests with the race detector
make test-cover-enforce  # fail if total coverage drops below the floor (COVER_MIN in the Makefile)
make lint                # golangci-lint run ./... (NEW_FROM_REV=<rev> limits to new issues only)
make vet                 # go vet ./...
make fmt / fmt-check     # gofmt + goimports -local mini-s3 (check = verify only)
make precommit           # build + vet + fmt-check + lint + test + test-cover-enforce + parity-test + mod-tidy-check
make check               # precommit + test-race + vuln + secrets (full local gate)
make e2e                 # scripts/e2e/run-e2e.sh (AWS-CLI-driven e2e suite)
```

`make lint NEW_FROM_REV=<rev>` reports only issues introduced since `<rev>`; use it
during a branch so pre-existing backlog does not block you. The coverage floor is
enforced by `make test-cover-enforce` (see `COVER_MIN` in the Makefile for the
current value and its history; every new user-facing feature must land with e2e
coverage in `scripts/e2e/` in the same change - see AGENTS.md).

## Architecture

This is a single-binary Go S3-compatible server (module `mini-s3`, Go 1.25)
implementing core S3 operations with AWS Signature Version 4 authentication.
The S3 protocol layer (sigv4, XML, bucket/object/multipart handlers, request
dispatch) moved into `internal/frontend/s3/` during the 2026-09
frontend-interface split; package `main` keeps the server entrypoint, config,
backend lookup, and process wiring:

| File (package main) | Contents |
|---|---|
| `main.go` | main(), http.Server wiring, TLS config, graceful shutdown |
| `config.go` | ServerConfig, loadConfig, credentials, shared constants |
| `frontends.go` / `s3_wiring.go` | frontend registry mounting and process seams |
| `backend_lookup.go` / `backend_lazy.go` | bucket-to-backend resolution |
| `actions.go` | bucket actions subsystem (event shell commands) |
| `storage.go` | atomic-write helpers (`writeFileAtomic`, `writeFileAtomicJSON`, `lockObject`) |

| Package (internal/) | Contents |
|---|---|
| `internal/frontend/s3` | The former root-level files: `sigv4.go`, `types.go`, `xml.go`, `bucket_handlers.go`, `object_handlers.go`, `multipart_handlers.go`, plus dispatch and the s3 frontend entry |
| `internal/frontend` | Frontend registry, capability and conformance seams |
| `internal/backend`, `internal/backend/fsbackend`, `internal/backend/conformance` | Storage backend interface, filesystem backend, conformance helpers |
| `internal/metadata` | ZFS sidecar metadata and parity tests |
| `internal/objectmodel` | Object model, encoded key paths, snapshot/header parity |
| `internal/auth` | Authenticator seam (placeholder; pluggable auth is an open issue) |

All object-data and metadata JSON writes go through the `storage.go` atomic
helpers (temp file in the same directory, fsync, rename), so a crash never leaves
a truncated object or metadata file. Per-key write serialization is via
`lockObject`; multipart uses `getMultipartLock`.

### Storage Layout

```
<dataDir>/                  # config.json "dataDir" (default: ./data/)
  <bucket>/                 # Auto-discovered bucket (or symlink to dir)
    !data/                  # SHADOW DATA DIRECTORY: object data lives here
      <encoded-key>         # percent-encoded object key ("/" and "%" encoded)
    .bucket-actions         # Optional per-bucket actions config (JSON5)
    .metadata/
      <object>.meta         # JSON metadata file per object
      .uploads/
        <uploadId>.json     # Multipart upload session metadata
        <uploadId>_parts/   # Part files during multipart upload

<custom-bucket-path>/       # Custom bucket from config.json "buckets" map
  !data/
    <encoded-key>
  .metadata/
    ...
```

Object data is stored under the `!data` shadow directory with percent-encoded
keys (see `internal/frontend/s3/object_paths.go` and
`internal/backend/fsbackend/paths.go`); only `.metadata/` sidecars sit beside it.

### Request Flow

The s3 frontend's handler (the former `rootHandler` chain, now mounted via the
frontend registry in `internal/frontend/s3/frontend.go` and `dispatch.go`) is
the single entry point that:
1. Parses path into bucket/object names
2. Calls `authenticateRequest` for SigV4 validation
3. Routes to operation handlers based on method and query params

### Key Components

- **SigV4 Authentication**: Full implementation including canonical request
  construction, signing key derivation, signature comparison, `aws-chunked`
  body decoding, and `x-amz-decoded-content-length` verification.
- **Object Metadata**: `ObjectMetadata` struct stored as JSON, tracks content
  type, ETag (MD5), custom x-amz-meta-* headers, and storage path.
- **Multipart Uploads**: Three-phase flow (initiate/upload parts/complete) with
  `MultipartUpload` tracking part ETags and temporary storage. Parts are written
  atomically; complete holds the object lock so a concurrent PUT cannot tear the
  final object.
- **XML responses**: `xml.Header` prolog plus the S3 namespace
  `http://s3.amazonaws.com/doc/2006-03-01/` pinned on every document
  (`s3XMLNamespace` in `xml.go`).
- **ListObjectsV2**: supports `encoding-type=url` key encoding.

### Configuration

The server loads configuration from `config.json` (or the path specified by the
`MINIS3_CONFIG` env var). See `config.json.example` for a commented sample.

```json
{
  "dataDir": "./data/",
  "listenAddr": ":8443",
  "certFile": "certs/cert.pem",
  "keyFile": "certs/key.pem",
  "buckets": {
    "photos": "/mnt/storage/photos",
    "backups": "/var/backups/s3-mirror"
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `dataDir` | `./data/` | Root directory for auto-discovered buckets |
| `listenAddr` | `:8443` | HTTPS listen address; `MINIS3_LISTEN_ADDR` env var overrides |
| `certFile` | `certs/cert.pem` | TLS certificate path |
| `keyFile` | `certs/key.pem` | TLS private key path |
| `buckets` | `{}` | Bucket name to custom filesystem path (bare string, or object `{ "path": ..., "backend": ... }`) |
| `backends` | `{}` | Backend type name to `{ "root": ..., "options": {...} }` construction config |

- **Custom buckets** appear in ListBuckets, cannot be created or deleted via the
  S3 API, and can point to any directory (including symlinks). Both the dataDir
  scan and custom bucket paths follow symlinks.
- **Backend selection** (per bucket): a bucket's `backend` key names a backend
  registered in the `internal/backend` registry (`"fs"` is built in via
  `fsbackend`'s `init()`); absent/`""` selects the default (`fs`). Root
  precedence: explicit bucket path > named backend's `root` > `dataDir`. An
  unknown backend name **aborts startup** (never a silent fallback to fs).
- **Graceful shutdown**: SIGINT/SIGTERM stops accepting new connections and
  drains in-flight requests for up to 30 seconds before exit (`main.go`).

### Credentials

One global credential pair, set via environment variables or defaulting to
`minioadmin`/`minioadmin`:
- `MINIS3_ACCESS_KEY` - Access Key ID
- `MINIS3_SECRET_KEY` - Secret Access Key
- `MINIS3_CONFIG` - Path to config file (default: `config.json`)
- `MINIS3_LISTEN_ADDR` - Overrides `listenAddr` (env beats config file)

Setting either credential variable to an empty string logs a warning and falls
back to the default; it does not disable default credentials.

### Testing with AWS CLI

```bash
aws s3 ls --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl
```

Configure profile with credentials `minioadmin`/`minioadmin` and region `us-east-1`.

## Known Issues / Documented Divergences

The historical data-loss and SigV4 bugs from `docs/gap-closure.md` are fixed.
What remains is a short list of intentional divergences from real S3:

- **Object keys containing `..` (or `.metadata`) path segments are rejected.**
  Real S3 treats keys as flat strings and permits `..`; here safety wins
  (`validateObjectKey` in `object_handlers.go`). `a..b` and `..hidden` are fine;
  only whole segments match.
- **Bucket-level traversal names are rejected** by `validBucket` /
  `validateBucketName` (`bucket_handlers.go`): names like `..` or `../escape`
  fail the S3 naming rules (3-63 chars, lowercase/digits/hyphens/periods) and
  never reach the filesystem path join.
- **Range requests: single ranges only.** GET/HEAD honor `Range` (206 with
  `Content-Range`, 416 `InvalidRange` for unsatisfiable requests - hardening
  leaf 3.1), and conditional `If-*` headers are evaluated first per RFC 7232.
  Multi-range requests are not supported: clients asking for several ranges get
  a full-body `200` instead of `206`/`multipart/byteranges`.
- **Single credential.** One access/secret pair for all clients; no per-user
  IAM, policies, or ACLs (ACL endpoints return "Not Implemented").
- **Region is pinned to `us-east-1`.** Requests signed for another region are
  rejected with `AuthorizationHeaderMalformed`/400 (matching AWS behavior).

See `docs/gap-closure.md` for the fix history and the 2026-09 hardening
campaign summary.
