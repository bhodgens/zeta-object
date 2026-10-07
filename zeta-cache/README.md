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

### IPC requests (leaf 07)

| Type | Effect |
|---|---|
| `status` | Live counters + quota fields (`usageBytes`, `capBytes`, `overflow`, `evicted`, `paused`). |
| `pause` / `resume` | Suspend/resume the three scheduler loops. An in-flight single-file upload finishes; the next sync pass holds at a whole-path boundary; a paused daemon still serves FUSE reads from cache. |
| `pin` (with `path` and `pin` bool) | Toggle the pinned flag; pinned paths are hard-filtered from eviction. |
| `pins` | List pinned paths. |
| `tombstones` | List the deletion-grace rows for the GUI's restore view (restore itself is leaf 08). |

Plan tree: `docs/plans/client-cache-2026-10/master.md` (scheduler leaf 07
owns `internal/scheduler`: prompt-upload ~2s debounce, sync every ~15min
with backoff + jitter capped at 1h + nightly full rescan, eviction
nightly + at the high-water mark, deferred on battery while discharging
on macOS).

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
| `caFile` | string | no | system pool | PEM bundle the gateway's server certificate is verified against (self-signed private gateway: point at its cert). |
| `insecureSkipVerify` | bool | no | `false` | Disables server-cert verification. There is no reason to set it outside throwaway loops; when set, startup logs a WARNING. |
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
gateway (PROPFIND Depth 0 on the bucket path, through the real
transport - the same TLS trust and auth material every later request
uses). A failure logs a WARNING and the daemon starts anyway - the local
mount keeps working against the cache, and sync retries when the server
returns.

## Transport (leaf 06)

The daemon speaks WebDAV to the gateway through `internal/transport`:

- **TCP path (always).** HTTPS to `serverUrl`, Basic auth
  (`accessKey`/`secretKey`), server certificates verified against
  `caFile` (or the system pool). PUT always sends `Content-Length` (the
  gateway rejects chunked bodies); conditional writes carry `If-Match` /
  `If-None-Match`; MOVE enforces `If-Match` on the destination.
- **HTTP/3 path (automatic, mTLS only).** The gateway stamps
  `alt-svc: h3="<port>"; persist=1` on every response. When the config
  supplies a client cert (`auth.clientCert`/`auth.clientKey`), the
  transport dials UDP/QUIC to the advertised port with the per-device
  cert (quic-go pinned to the gateway's exact version). There is no HTTP
  401 over h3: a missing/wrong cert fails the TLS handshake, surfaced as
  `ErrAuthFailed` (a quic-go crypto TransportError).
- **Sticky fallback.** An h3 dial/handshake failure degrades to TCP for
  the next 5 minutes (a per-client state machine:
  `tcp-only -> eligible -> active`, with `fallback` re-arming `eligible`
  after the window). Only PRE-SEND failures may fall back - a request
  that reached the wire is never replayed, so a PUT can never apply
  twice (the no-double-apply rule; leaf 07's scheduler owns all other
  retry policy, and the one in-transport retry is the idempotent-read
  reconnect on connection reset).

### Issuing a per-device client certificate

The gateway's h3 frontend trusts the CAs in its `clientCAFile`. Issue one
cert per device (CN = the device's identity name) against that CA:

    # one-time: create the client CA (its PEM is the gateway's clientCAFile)
    openssl req -x509 -newkey rsa:2048 -keyout client-ca-key.pem \
        -out client-ca.pem -days 365 -nodes -subj '/CN=zeta-client-ca'

    # per device:
    openssl req -newkey rsa:2048 -keyout device-key.pem \
        -out device.csr -nodes -subj '/CN=<device-identity-name>'
    printf 'keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > device.ext
    openssl x509 -req -in device.csr -CA client-ca.pem \
        -CAkey client-ca-key.pem -CAcreateserial \
        -out device.pem -days 365 -extfile device.ext

Point `auth.clientCert`/`auth.clientKey` at `device.pem`/`device-key.pem`.

### Keychain/keyring storage (v1 tradeoff)

Certificate material is loaded through the `transport.CertStorage`
interface; v1 ships only the file-backed implementation
(`FileCertStore`, PEM files, key mode 0600). An OS keychain
(macOS Keychain, Linux secret-service) would protect the private key
with the user session and touch-to-use prompts, but every Go binding is
cgo - banned repo-wide - or needs an external daemon. The seam is the
extension point: a future `KeychainCertStore` implements
`LoadClientCert`/`TrustRoots` and drops in without touching the client.
