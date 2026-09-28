# Minimalist S3 Server in Go

This project implements a minimalist S3-compatible server in Go. It supports core bucket and object operations, including multipart uploads, and uses AWS Signature Version 4 for authentication.

## Why mini-s3?

**Serve any directory as an S3 bucket.** Unlike other S3-compatible servers that manage their own opaque storage, mini-s3 maps buckets directly to filesystem directories. This means you can:

- Point a bucket at an existing directory and immediately access its contents via S3 API
- Use symlinks to expose directories from anywhere on your system
- Mount network shares, NFS volumes, or any filesystem and serve them as S3 buckets
- Mix auto-discovered buckets with explicitly configured custom paths

This makes mini-s3 ideal for:
- Exposing existing file archives via S3-compatible APIs
- Creating S3 gateways to legacy storage systems
- Development and testing against real directory structures
- Serving static content from arbitrary locations

## Features

*   **Bucket Operations**:
    *   Create Bucket (`PUT /<bucket>`)
    *   List Buckets (`GET /`)
    *   Delete Bucket (`DELETE /<bucket>`)
    *   Get Bucket Location (`GET /<bucket>?location`)
    *   HEAD Bucket (`HEAD /<bucket>`)
*   **Object Operations**:
    *   Upload Object (`PUT /<bucket>/<object>`)
    *   Get Object (`GET /<bucket>/<object>`)
    *   Delete Object (`DELETE /<bucket>/<object>`)
    *   List Objects (`GET /<bucket>?list-type=2`)
        *   Supports `prefix`, `delimiter`, `continuation-token`, `start-after`, `max-keys`, and `encoding-type=url` key encoding.
    *   HEAD Object (`HEAD /<bucket>/<object>`)
*   **Multipart Uploads**:
    *   Initiate (`POST /<bucket>/<object>?uploads`)
    *   Upload Part (`PUT /<bucket>/<object>?partNumber=N&uploadId=XYZ`, parts 1-10000)
    *   Complete (`POST /<bucket>/<object>?uploadId=XYZ`) - final object Content-Type comes from the initiate request
    *   Abort (`DELETE /<bucket>/<object>?uploadId=XYZ`)
*   **Authentication**: AWS Signature Version 4 (HMAC-SHA256), including `aws-chunked` payload decoding and `x-amz-decoded-content-length` verification for streaming uploads.
*   **Data integrity**: all object data and metadata files are written atomically (temp file + fsync + rename), and concurrent writes to the same object are serialized per key. A crash mid-write never leaves a truncated object behind.
*   **Path traversal safety**: object keys containing `..` or `.metadata` path segments are rejected, and bucket names must pass S3 naming rules, so a request can never escape its bucket directory.
*   **XML conformance**: every XML document carries the XML prolog and the S3 namespace `http://s3.amazonaws.com/doc/2006-03-01/`; errors use real S3 error codes and HTTP statuses.
*   **Flexible Storage**: Maps buckets directly to filesystem directories. Auto-discovers buckets from a configurable root directory, plus supports explicit bucket-to-path mappings for serving arbitrary directories. Follows symlinks.
*   **HTTPS**: HTTPS-only with a TLS 1.2 minimum; the bundled `make certs` certificate includes SANs for `localhost` and `127.0.0.1`. Graceful shutdown on SIGINT/SIGTERM drains in-flight requests (up to 30s).
*   **Event Actions**: Execute shell commands in response to S3 operations (upload, download, delete). Supports glob pattern matching, async execution, configurable timeout (30s default), output capture (truncated at 1MB per stream), and inactivity triggers. Variable values are shell-quoted automatically. Configure via `.bucket-actions` files.
*   **ACLs**: Stubbed (returns "Not Implemented").

## Quickstart

```bash
make hooks        # one-time: install git hooks (pre-commit quality gates)
make build        # compile ./mini-s3-server
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
make e2e          # end-to-end suite (lands with hardening leaf 3.6)
```

## Credentials Configuration

The server uses **AWS Signature Version 4** for authentication. You must configure matching credentials on both the server and client.

The server has a **single global credential pair** - there are no per-user credentials, IAM users, or policies.

### Server-Side Credentials

Credentials can be set via environment variables (recommended) or will fall back to defaults:

| Environment Variable | Default Value | Description |
|---------------------|---------------|-------------|
| `MINIS3_ACCESS_KEY` | `minioadmin`  | Access Key ID |
| `MINIS3_SECRET_KEY` | `minioadmin`  | Secret Access Key |

Setting either variable to an empty string logs a warning and falls back to the default - it does not disable default credentials.

**Option 1: Use default credentials (quickstart)**

The server ships with default credentials `minioadmin`/`minioadmin`. Just start the server and configure your client to match.

**Option 2: Set custom credentials via environment**

```bash
export MINIS3_ACCESS_KEY="myaccesskey"
export MINIS3_SECRET_KEY="mysecretkey"
./mini-s3-server
```

### Client-Side Configuration (AWS CLI)

**Step 1: Create an AWS CLI profile**

```bash
aws configure --profile minis3
```

Enter the following when prompted:
```
AWS Access Key ID [None]: minioadmin
AWS Secret Access Key [None]: minioadmin
Default region name [None]: us-east-1
Default output format [None]: json
```

The region **must** be `us-east-1`: the server is pinned to that region and rejects requests signed for any other region with `AuthorizationHeaderMalformed`.

**Step 2: Test the connection**

```bash
aws s3 ls --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl
```

## Configuration

mini-s3 uses a JSON configuration file (see `config.json.example` for a commented sample). By default it looks for `config.json` in the current directory; change the path with `MINIS3_CONFIG`.

### config.json keys

| Key | Default | Description |
|-----|---------|-------------|
| `dataDir` | `./data/` | Root directory for auto-discovered buckets. Any subdirectory (including symlinks) becomes a bucket. |
| `listenAddr` | `:8443` | HTTPS listen address (host:port). |
| `certFile` | `certs/cert.pem` | TLS certificate path. |
| `keyFile` | `certs/key.pem` | TLS private key path. |
| `buckets` | `{}` | Map of bucket names to custom filesystem paths. |

Credentials are **not** set in the config file - environment variables only.

### Environment variables

| Variable | Description | Default |
|----------|-------------|---------|
| `MINIS3_CONFIG` | Path to configuration file | `config.json` |
| `MINIS3_LISTEN_ADDR` | Overrides `listenAddr` (env beats config file) | - |
| `MINIS3_ACCESS_KEY` | Access Key ID for authentication | `minioadmin` |
| `MINIS3_SECRET_KEY` | Secret Access Key for authentication | `minioadmin` |

### How Bucket Discovery Works

1. **Custom buckets** (from `config.json` `buckets` map) are loaded first
2. **Auto-discovered buckets** are found by scanning `dataDir` for subdirectories
3. Symlinks are followed - a symlink to a directory is treated as a valid bucket
4. Custom buckets take precedence if there's a name collision

### Custom Bucket Behavior

Buckets defined in `config.json`:
- Appear in `ListBuckets` alongside auto-discovered buckets
- **Cannot be deleted** via the S3 API (protected)
- **Cannot be created** via the S3 API (already exist)
- Can point to any readable directory on the filesystem

### Example: Serving Existing Directories

To expose `/var/log` as an S3 bucket named `logs` and `/home/user/documents` as `docs`:

```json
{
  "dataDir": "./data/",
  "buckets": {
    "logs": "/var/log",
    "docs": "/home/user/documents"
  }
}
```

Then access via S3:
```bash
aws s3 ls s3://logs/ --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl
aws s3 cp s3://docs/report.pdf ./report.pdf --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl
```

## Setup and Installation

1.  **Clone the Repository**:
    ```bash
    git clone <repository-url>
    cd mini-s3
    ```

2.  **Install git hooks** (recommended for development):
    ```bash
    make hooks
    ```

3.  **Generate SSL Certificates**:
    The server requires TLS certificates (`cert.pem` and `key.pem`) to run over HTTPS. The Makefile generates self-signed certificates with SANs for `localhost` and `127.0.0.1`:
    ```bash
    make certs
    ```
    Certificates are only generated if missing; delete them and re-run to regenerate. If you have your own certificates, place them in the `certs` directory (or point `certFile`/`keyFile` at them).

4.  **Create Data Directory** (optional):
    The server creates the data directory on startup if it doesn't exist.

## Building and Running

*   **Build the Server**:
    ```bash
    make build
    ```
    This compiles the Go program and creates an executable named `mini-s3-server` in the project root.

*   **Run the Server**:
    ```bash
    make run
    ```
    This builds the server and starts it. By default the server listens on `https://localhost:8443` (override with `listenAddr` or `MINIS3_LISTEN_ADDR`). SIGINT/SIGTERM trigger a graceful shutdown that drains in-flight requests for up to 30 seconds.

*   **Clean Build Artifacts**:
    ```bash
    make clean
    ```

## Using with an S3 Client

When configuring your client:

*   **Endpoint URL**: Set this to `https://localhost:8443` (or your `listenAddr`).
*   **Access Key ID / Secret**: Use `minioadmin`/`minioadmin` (default) or your custom credentials.
*   **Region**: Set this to `us-east-1` - other regions are rejected.
*   **SSL Verification**: The default self-signed certificate includes SANs for `localhost` and `127.0.0.1`, so clients that trust the cert work without flags; otherwise use `--no-verify-ssl` with `aws-cli` for local testing.

**Example with `aws-cli`**:

```bash
# List buckets
aws s3 ls --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl

# Create a bucket
aws s3 mb s3://mytestbucket --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl

# Upload a file
aws s3 cp test.txt s3://mytestbucket/test.txt --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl

# List objects in a bucket
aws s3 ls s3://mytestbucket --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl

# Download a file
aws s3 cp s3://mytestbucket/test.txt downloaded_test.txt --profile minis3 --endpoint-url https://localhost:8443 --no-verify-ssl
```

## Project Structure

```
.
├── main.go              # main(), server wiring, TLS, graceful shutdown
├── config.go            # configuration + credentials
├── sigv4.go             # AWS SigV4 auth + aws-chunked decoding
├── bucket_handlers.go   # bucket operations
├── object_handlers.go   # object operations
├── multipart_handlers.go# multipart upload lifecycle
├── xml.go               # XML error/response helpers
├── actions.go           # bucket actions subsystem
├── storage.go           # atomic-write helpers + per-key locking
├── types.go             # shared structs (metadata, XML documents)
├── Makefile             # build, run, test, quality gates
├── config.json.example  # Example configuration file
├── certs/               # SSL certificates (generated by 'make certs')
└── data/                # Default root for auto-discovered buckets
    └── <bucket>/        # Each subdirectory is a bucket
        ├── <objects>    # Object data files
        └── .metadata/   # Object metadata (JSON files)
```

## Testing

Run unit tests:
```bash
make test
```

Run the full local quality gate (build, vet, fmt, lint, tests, race, vuln, secrets):
```bash
make check
```

Run integration tests (requires server to be running):
```bash
./scripts/test-s3-operations.sh
```

## Development History

See [docs/gap-closure.md](docs/gap-closure.md) for the history of issues that were identified and fixed during development, including the 2026-09 hardening campaign.

## Event Actions

mini-s3 supports configurable event-based actions that execute shell commands in response to S3 operations. Actions are configured via `.bucket-actions` files (JSON5 format) placed in bucket directories.

### Use Cases

- **Post-upload processing**: thumbnail generation, video transcoding, backup notifications
- **Delete-after-download**: ephemeral file cleanup
- **Inactivity snapshots**: ZFS snapshots after periods of no activity
- **Audit logging**: track all downloads/uploads

### Configuration

Place a `.bucket-actions` file in any bucket directory or subdirectory. Subdirectories inherit parent actions and can override them. See `bucket-actions.sample.json5` for a complete example.

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
  "after_download": [],
  "after_delete": [],
  "inactivity_timeout": {
    "duration": "30m",
    "command": "zfs snapshot tank/data@$(date +%s)",
    "reset_on": ["upload", "delete"]
  }
}
```

### Available Variables

| Variable | Description |
|----------|-------------|
| `$FILE_PATH` | Full path to object data file |
| `$METADATA_PATH` | Path to `.metadata/<obj>.meta` |
| `$BUCKET_NAME` | S3 bucket name |
| `$BUCKET_PATH` | Filesystem path to bucket root |
| `$OBJECT_KEY` | Object key within bucket |
| `$CONTENT_TYPE` | MIME type |
| `$ETAG` | Object ETag (MD5) |
| `$SIZE` | Size in bytes |

### Inheritance

When an object operation occurs, actions are loaded from the bucket root through all parent directories to the object's location. Each child directory's `inheritance.mode` controls how it combines with its parent:

| Mode | Behavior |
|------|----------|
| `merge` (default) | Child actions added to parent actions; same-name child actions override the parent's |
| `override` | Child completely replaces parent actions |
| `disable` | No actions run in this subtree - neither the parent's actions nor this directory's own actions execute |

### Security Notes

1.  **Shell-quoting semantics**: Every substituted variable value is automatically shell-quoted (POSIX single-quote escaping), so values from object keys or metadata can never break out of quoting or inject commands. **Reference variables unquoted** in your command templates (`/script.sh $FILE_PATH`, not `"$FILE_PATH"`) - wrapping them in your own quotes adds extra literal quote characters to the argument. Text inside a substituted value is never re-expanded (single-pass substitution).
2.  **Command timeout**: 30 seconds by default; override per action with `timeout` (seconds). On timeout the whole process group is killed.
3.  **Output capture**: Combined stdout/stderr is logged per stream, truncated at 1MB.
4.  **Permissions**: Commands run as the server process user - an action can do anything the server can.
5.  **Async**: Actions run asynchronously by default (non-blocking); set `"async": false` to block the response.
6.  **Single credential + TLS 1.2 floor**: One credential pair protects all operations; keep `MINIS3_SECRET_KEY` out of shared shells and never disable the HTTPS listener.
7.  **Path traversal safety**: Object keys containing `..` or `.metadata` segments are rejected and bucket names must pass S3 naming rules, so actions can never be triggered on files outside the bucket tree.

### Discovery Script

Use `scripts/show-bucket-actions.sh` to view all configured actions:

```bash
./scripts/show-bucket-actions.sh data/
```

## Known Limitations

*   No Range/conditional requests (GET) - requests always return the full object with 200.
*   Single credential; no per-user auth, ACLs, or bucket policies.
*   Region pinned to `us-east-1`.
*   Object keys with `..` or `.metadata` path segments are rejected (safety over S3 compatibility).
*   No versioning or lifecycle policies.

## TODO / Potential Enhancements

*   Range and conditional (If-Match/If-None-Match) request support.
*   Presigned URL authentication.
*   Multipart lifecycle APIs (ListMultipartUploads, ListParts, upload expiry).
*   CopyObject and batch DeleteObjects.
*   Verified streaming chunk signatures.
*   Implementation of ACLs and Bucket Policies.
*   Versioning and lifecycle policies.
*   More detailed logging options.
