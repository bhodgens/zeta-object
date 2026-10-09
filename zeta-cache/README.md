# zeta-cache

`zeta-cache` is the client-side FUSE sync daemon for zeta-object. It makes a
remote zeta-object bucket (spoken over WebDAV) look like a local directory,
keeps a local cache of file contents, and syncs changes both ways. All
cache, eviction, and sync state lives on the client - the gateway stores
nothing about who cached what (charter).

Implementation status: leaf 08 (GUI + IPC). The daemon mounts (FUSE),
syncs both ways (ETag-diff scan + conflict copies), runs the three
scheduler loops (prompt upload / periodic sync / quota eviction), serves
the full v1 IPC protocol, and ships with the macOS menu-bar app
(`gui/macos/`, below). The event-cursor optimization (leaf 05) is
parked pending zeta-object#15; v1 ships scan-only.


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

## IPC protocol (v1, full reference)

JSON over a Unix domain socket (mode 0600, default
`<runtime dir>/zeta-cache.ipc`; the macOS runtime dir is `/tmp` — see
`ipcSocket` in the config table). One request per connection, one JSON
object each way, newline-terminated on the response. Every message
carries `v` (protocol version, currently `1`).

Response envelope — success:

    {"v":1,"ok":true,"data":{...}}     // status, pause, resume
    {"v":1,"ok":true,"extra":{...}}    // everything else

failure:

    {"v":1,"ok":false,"error":"<human-readable reason>"}

An unsupported `v` and an unknown `type` both answer
`{"v":1,"ok":false,"error":"unknown request type"}`-style errors
(forward compatibility: a newer client must never crash an older
daemon, and vice versa). A daemon started without the sync engine
(mount/scheduler construction failed) answers the state-changing types
with `not supported: ...` capability errors instead of hanging.

### Request reference

| Type | Extra fields | Effect | Success payload |
|---|---|---|---|
| `status` | - | Live snapshot of daemon state. | `data`: `state` (`idle`/`syncing`/`error`), `server`, `bucket`, `lastSync` (unix secs or null), `dirty`, `cached`, `conflicts`, `paused`, `usageBytes`, `capBytes`, `overflow`, `evicted`, `lastEvictAt` (unix secs or null), `lastError` (omitted when empty). |
| `pause` | - | Suspend all three scheduler loops. An in-flight single-file upload FINISHES; the running sync pass finishes at whole-path boundaries; the next passes hold. A paused daemon still serves FUSE reads from cache. | `data`: full status with `paused: true`. |
| `resume` | - | Restart the loops. | `data`: full status with `paused: false`. |
| `pin` | `path`, `pin` (bool) | Toggle the pinned flag; pinned paths are hard-filtered from eviction. | `extra`: `{"pins":[...]}` (list after the change). |
| `pins` | - | List pinned paths. | `extra`: `{"pins":["a","b"]}`. |
| `tombstones` | - | List the deletion-grace rows (leaf-07 name for the deleted view). | `extra`: `{"tombstones":[{"path","deletedAt","expiresAt"}]}` (unix secs). |
| `conflicts.list` | - | List pending conflict records (matrix-3 conflict copies and matrix-5 kept-dirty-on-remote-delete). | `extra`: `{"conflicts":[{"path","kind","copyPath"?}]}`; `kind` is `conflict-copy` or `kept-local`; `copyPath` is the preserved local file (absent for `kept-local`). |
| `conflicts.resolve` | `path`, `mode`, `confirm` (bool) | Resolve one conflict. `keep-local` = the local version wins (its bytes are uploaded, overwriting the server copy). `keep-remote` = the server version wins (downloaded over the local path). The preserved conflicted-copy FILE is deleted ONLY when `confirm` is true (explicit user action; never implicit). | `extra`: `{"path","mode","copyRemoved"}`. |
| `deleted.list` | - | List tombstoned paths within the deletion-grace window. | `extra`: same shape as `tombstones`. |
| `deleted.restore` | `path` | Re-download a deleted path if the server still serves it (tombstone flips back to a clean hydrated row). An unrecoverable path is an HONEST error ("the server no longer serves the path"), never a silent ok. | `extra`: `{"path"}`. |
| `evict` | `path` | Evict ONE clean file from the cache now (admin-ish). Refusals (surfaced as errors): dirty (unsynced local edits), pinned, tombstoned, not hydrated, unknown path. | `extra`: `{"path"}`. |
| `enumerate` | `path` (dir; `""` = root; trailing slash optional) | List the DIRECT children of a dir from the index (the sync's cached view; never the network). Tombstoned rows never surface; deeper rows collapse into their immediate subdirectory entry. Unknown dir answers ok with EMPTY entries (never null). Requires the sync engine. | `extra`: `{"entries":[{"name","key","isDir","size","mtime","materialized","dirty","cachePath"}]}`. `key` is the server key (the FileProvider item identity); `mtime` is unix secs (0 = unknown); `cachePath` is the backing file RELATIVE to `cacheDir` (`"files/<key>"`, `""` for dirs). |
| `item` | `path` | Single-entry metadata (the FileProvider Lookup analog). Unknown/tombstoned path: `no such item: <path>` error. | `extra`: `{"item":{...same shape as one enumerate entry...}}`. |
| `download` | `path` | BLOCKING hydration through the engine's download path: returns when the cache file is materialized or the 30s request deadline fires. Already-materialized paths answer without a sync. Unknown path: honest error. | `extra`: `{"path","materialized","cachePath"}`. |
| `dehydrate` | `path` | Evict ONE clean file keyed by path (same refusals as `evict`: dirty/pinned/tombstoned/not-hydrated/unknown). The FileProvider "Remove Download" action lands here. | `extra`: `{"path"}`. |
| `mark` | `path` | Flag an already-written cache file dirty so prompt-upload pushes it (the extension writes bytes BEFORE mark). Creates/refreshes the row from the DISK truth (size/mtime); dirty rows keep their known-good ETag (If-Match on upload). Missing cache file: error. | `extra`: `{"path","dirty":true}`. |
| `delete` | `path` | Tombstone the path: cache file removed + tombstone row (the sync's matrix-6 pass issues the remote DELETE). Unknown paths are fine (tombstone either way). | `extra`: `{"path"}`. |
| `move` | `path`, `to` | Rename: cache-file rename + index row re-point (old key tombstoned; the sync deletes it remotely and uploads the new key). Unknown source: `no such item` error. | `extra`: `{"from","to"}`. |
| anything else | - | Unknown type. | `{"v":1,"ok":false,"error":"unknown request type"}`. |

The seven `enumerate`/`item`/`download`/`dehydrate`/`mark`/`delete`/`move`
types dispatch through the ADDITIVE `FileProviderSource` capability seam
(`internal/ipc/fileprovider.go`, the leaf-08 `ConflictResolver` pattern): a
daemon whose handler does not implement it answers them with
`not supported: no file provider handler`, keeping the older surfaces and
the unknown-type rejection byte-identical.

### The data-via-FILE, metadata-via-IPC contract (FileProvider)

The FileProvider extension (`gui/fileprovider/`, leaf 02 of
`docs/plans/fileprovider-2026-10/`) is a thin adapter over this daemon:

- METADATA via IPC: enumerate/item/download/dehydrate/mark/delete/move -
  exactly the table rows above.
- DATA via the FILE: every entry carries `cachePath` (relative to
  `cacheDir`), and the extension - same user - opens
  `<cacheDir>/<cachePath>` directly. No bytes ever cross the socket; the
  extension holds no credentials and makes no network calls.
- CREATE: the extension writes the bytes into `<cacheDir>/files/<key>`
  itself, then IPC `mark` (dirty) - the prompt-upload loop pushes them.
- Download trigger: Finder opens a `.downloadLazily` item -> IPC
  `download` (blocking hydrate) -> serve bytes from the cache file.
- Eviction: Finder "Remove Download" -> IPC `dehydrate`.

### Golden fixtures (protocol conformance)

The byte-exact Go encoder responses for every request type are pinned in
`internal/ipc/testdata/golden/*.json`. The Swift app decodes the SAME
files in its test suite (`gui/macos/Tests`), so a fixture change that
breaks the GUI fails a test instead of shipping:

    cd zeta-cache && go test ./internal/ipc -run TestGoldenFixtures -update   # regenerate
    cd zeta-cache/gui/macos && swift test                                    # Swift menu app side
    cd zeta-cache/gui/fileprovider && swift test                             # Swift FileProvider side (same fixtures, copied into its Tests/Fixtures/)

The gui/fileprovider package COPIES the Codable structs (SPM cannot
cross-import gui/macos); the duplication rule is documented at the top of
its IPC.swift: regenerate fixtures, then update BOTH Swift copies.

## GUI: macOS menu-bar app

`gui/macos/` is a Swift/SwiftUI SPM package (`swift-tools-version 5.9`,
macOS 13+). It is a pure view over the IPC socket: NO network access,
NO credentials, NO config write access — every network operation and
every decision lives in the daemon (one network-attached process; the
socket is mode 0600 in the user's runtime dir).

Menu items: state line (synced / syncing / error + last sync), usage vs
quota (with a bar when capped), pause/resume toggle, Conflicts submenu
(count → per-file resolve actions: keep mine / keep server's, plus
explicit "and delete my copy"), Pins submenu (unpin actions), Recently
Deleted submenu (restore actions), and Quit — which disconnects the GUI
ONLY; the daemon keeps running (as a login item, below).

Build and run:

    cd zeta-cache/gui/macos
    swift build
    .build/debug/ZetaCacheMenu &

The app reads the socket path from `ZETA_CACHE_IPC` (defaults to
`/tmp/zeta-cache.ipc`, the macOS default `ipcSocket`). Protocol
conformance runs with `swift test` (11 tests over the golden fixtures).

Packaging note (v1): an unsigned local build is fine for personal use;
signed/notarized distribution is future work.

## Run the daemon at login (LaunchAgent)

Two equivalent plist artifacts exist (same content, different location in
the tree):

- `gui/macos/com.zeta-object.zeta-cache.plist` - the leaf-08 menu-app setup.
- `gui/launchagents/com.zeta-object.zeta-cache.plist` - the
  fileprovider-2026-10 leaf-01 copy for the FileProvider setup (the
  extension talks ONLY to this daemon's socket + cache dir, so the daemon
  must run at login before Finder touches the domain).

Neither is installed automatically. One-time setup:

    # 1. put the binary somewhere stable
    cp zeta-cache-bin /usr/local/bin/            # or any PATH-free location

    # 2. edit the plist: fix BOTH absolute paths
    #    (binary + your zeta-cache.json), then:
    cp gui/macos/com.zeta-object.zeta-cache.plist \
       ~/Library/LaunchAgents/

    # 3. load it
    launchctl load ~/Library/LaunchAgents/com.zeta-object.zeta-cache.plist

    # stop / unload:
    launchctl unload ~/Library/LaunchAgents/com.zeta-object.zeta-cache.plist

The agent runs at login (`RunAtLoad`), restarts the daemon if it exits
(`KeepAlive`, 10 s throttle), and is scoped to the Aqua session (the
FUSE mount and IPC socket are per-user). The menu-bar app can quit and
relaunch freely; the daemon outlives it.

## Linux tray app

DEFERRED (plan-tree decision): the Linux tray app lands after the
daemon stabilizes. The IPC protocol is deliberately platform-neutral
(no macOS-only fields), so the future Linux app speaks the exact same
v1 protocol documented above.

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
