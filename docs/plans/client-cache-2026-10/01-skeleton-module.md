# Leaf 01 - zeta-cache module skeleton + daemon lifecycle

## Goal

Create `zeta-cache/` (own Go module) with the daemon skeleton: config,
lifecycle, package layout, e2e shell, CI wiring. No sync, no FUSE yet -
this leaf's deliverable is a `zeta-cache` binary that starts, reads
config, opens (creates) its index DB path, runs an idle loop, handles
SIGTERM/SIGINT cleanly, and exposes a Unix IPC socket answering a
`status` request.

## Requirements

- `zeta-cache/go.mod`: module `github.com/bhodgens/zeta-object/zeta-cache`,
  go directive matching the gateway's (1.27.0). Dependencies:
  modernc.org/sqlite (pinned same major as the gateway), net/http stdlib.
  NO cgo anywhere - static-binary property is repo law. Verify with a
  CGO_ENABLED=0 build.
- Config file (`zeta-cache.json` + `config.example.json`), read at
  startup, fail-loud on unknown keys (mirror the gateway's validator
  behavior): server URL, bucket, credentials ( EITHER access+secret
  key pair for Basic OR clientCert/clientKey paths for mTLS),
  cacheDir (default per-OS app-data), indexDB path (default
  cacheDir/index.db), mountpoint, quota settings (defer to leaf 07 for
  enforcement - fields exist now), log level, ipcSocket path.
- Daemon lifecycle in main.go: parse flags (config path, -version),
  load config, open/create index DB (leaf 03 owns the schema - this
  leaf just opens/migrates a placeholder v0), start the IPC socket
  (Unix domain socket, one request/response JSON protocol versioned
  with a `v` field - leaf 08 owns the fuller protocol), install signal
  handlers, run idle until signalled, shutdown order: stop IPC, close
  DB, unmount if mounted (leaf 02 fills this), exit 0.
- `status` IPC response (leaf 01 shape): `{v:1, state:"idle|syncing|error",
  server:"<url>", bucket:"<b>", lastSync:<rfc3339|null>, dirty:<n>,
  cached:<n>}` - fields used by later leaves; GUI comes in leaf 08.
- Logging: stdlib log, level from config, consistent prefix
  `zeta-cache:`.
- Package layout (leaf 01 creates the dirs, later leaves fill them):
  `internal/config`, `internal/index`, `internal/sync`, `internal/fusefs`,
  `internal/transport`, `internal/ipc`, `internal/gui` (empty placeholder
  packages with a doc.go each EXCEPT index and ipc which leaf 01
  implements).
- e2e shell: `scripts/e2e/cases/40-zeta-cache.sh` - builds the zeta-cache
  binary, starts the shared server, runs `zeta-cache --version` and a
  config-validation smoke (valid config starts and answers status;
  unknown key aborts with exit != 0), then SKIPs the mount sections
  with the documented skip message until leaf 02 lands. Follow the
  private-server pattern of case 19. Allocates case 40 (39 is taken).
- CI: extend the workflow's e2e job (or the build job) to build the
  zeta-cache binary CGO_ENABLED=0 linux/amd64 so the case can run.
- Makefile: `zeta-cache` target building the module to `./zeta-cache-bin`
  (gitignore it); `make clean` removes it. The gateway Makefile lives at
  repo root - edit it, do not shadow it.
- README stub `zeta-cache/README.md`: what it is, build, run, config
  reference (every key), link to the plan tree. House copy rules: hyphens
  not em-dashes, mechanism before name, no marketing words.

## Constraints

- Repo-root module untouched except the Makefile snippet and workflow
  e2e line. The gateway module must not gain zeta-cache as a dependency
  (independent modules; the e2e case shells out to the built binary).
- Charter: nothing in this leaf sends anything to the server beyond
  OPTIONS (a connectivity probe is allowed and expected at startup:
  one authenticated request, honest error state if it fails - the daemon
  still starts and mounts).
- Windows: not a target; no build-tag work for it.

## Acceptance

1. `cd zeta-cache && CGO_ENABLED=0 go build ./...` green.
2. `./zeta-cache-bin --config valid.json` starts, `status` IPC returns
   the v1 shape, SIGTERM exits 0.
3. Unknown config key aborts non-zero with the offending key named.
4. e2e case 40 green in `make e2e` (build + config smoke + graceful
   mount skip), full suite still green (report the tally).
5. `make lint NEW_FROM_REV=HEAD` 0 issues (the repo linter covers
   `scripts/`; if it does not reach `zeta-cache/`, run golangci-lint in
   the subdirectory and report that).
6. CGO_ENABLED=0 build proven in the report (no cgo creeps in).
