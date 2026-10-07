# zeta-cache

`zeta-cache` is the client-side FUSE sync daemon for zeta-object. It makes a
remote zeta-object bucket (spoken over WebDAV) look like a local directory,
keeps a local cache of file contents, and syncs changes both ways. All
cache, eviction, and sync state lives on the client - the gateway stores
nothing about who cached what (charter).

Implementation status: this is the leaf 01 skeleton. The daemon starts,
reads config, opens its index DB, answers `status` on a local IPC socket,
and shuts down cleanly. The FUSE mount (leaf 02), sync engine (leaf 04),
quota and eviction (leaf 07), and menu-bar GUI (leaf 08) are not here yet.

Plan tree: `docs/plans/client-cache-2026-10/master.md`.

## Build

`zeta-cache` is its own Go module inside this repo (independent of the
gateway module). Static binaries only - cgo is banned repo-wide, so SQLite
is reached through `modernc.org/sqlite`:

    make zeta-cache          # builds ./zeta-cache-bin at the repo root
    cd zeta-cache && go test -race ./...

## Run

    ./zeta-cache-bin --config zeta-cache.json
    ./zeta-cache-bin --version

The daemon logs with the prefix `zeta-cache:`, serves one JSON request per
connection on a Unix socket (mode 0600), and exits 0 on SIGINT/SIGTERM in
order: stop IPC, close the index DB, unmount (stub until leaf 02).

### IPC status

Send `{"v":1,"type":"status"}` to the socket. Leaf 01 answers:

    {"v":1,"ok":true,"data":{"state":"idle","server":"<serverUrl>",
     "bucket":"<bucket>","lastSync":null,"dirty":0,"cached":0,"conflicts":0}}

An unknown type answers `{"v":1,"ok":false,"error":"unknown request type"}`.
Leaf 08 owns the fuller protocol; `state` stays `idle` until the sync
leaves land.

## Configuration

The config file is JSON with fail-loud validation: an unknown key aborts
startup naming the key. See `config.example.json` (Basic auth) and
`config.example-mtls.json` (mTLS auth).

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `serverUrl` | string | yes | - | Base URL of the zeta-object gateway. |
| `bucket` | string | yes | - | Bucket to sync. |
| `auth` | object | yes | - | Credentials; EXACTLY ONE shape (below). |
| `auth.accessKey` / `auth.secretKey` | string | one shape | - | Basic credentials for TCP/WebDAV. |
| `auth.clientCert` / `auth.clientKey` | string | one shape | - | Paths to a per-device client certificate and key for mTLS (HTTP/3). |
| `cacheDir` | string | no | per-OS app-data path | Where cached file contents live. macOS: `~/Library/Application Support/zeta-cache`; Linux: `~/.local/share/zeta-cache`. |
| `indexDB` | string | no | `<cacheDir>/index.db` | SQLite index (placeholder in leaf 01; real schema is leaf 03). |
| `mountpoint` | string | yes | - | Where the FUSE filesystem mounts (leaf 02). |
| `ipcSocket` | string | no | per-OS runtime dir / `zeta-cache.ipc` | Unix socket for the status protocol. Parent dir is created; a stale socket file is removed at startup. |
| `quota.maxCacheBytes` | int | no | 0 (unset) | Cache size cap. Parsed now; enforced by leaf 07. |
| `quota.minFreeDeviceBytes` | int | no | 0 (unset) | Free-space hard stop. Parsed now; enforced by leaf 07. |
| `quota.policy` | string | no | - | `lru`, `lifo`, or `pinned-first`. Validated now; used by leaf 07. |
| `logLevel` | string | no | `info` | `debug`, `info`, `warn`, or `error`. |
| `pins` | []string | no | `[]` | Paths that eviction must never remove. Parsed now; honored by leaf 07. |

## Startup connectivity probe

At startup the daemon sends exactly one authenticated request to the
gateway (OPTIONS on the bucket path). A failure logs a WARNING and the
daemon starts anyway - the local mount keeps working against the cache,
and sync retries when the server returns.
