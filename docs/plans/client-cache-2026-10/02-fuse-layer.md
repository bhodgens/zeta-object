# Leaf 02 - FUSE filesystem layer

## Goal

`internal/fusefs`: a FUSE mount (bazil.org/fuse or go-fuse - pick ONE,
justify in the report; both are pure Go; macFUSE/libfuse is the runtime
dependency either way) that serves the bucket namespace backed by the
local cache dir, the index DB (leaf 03), and the transport (leaf 06,
stubbed behind an interface until it lands).

## Requirements

- Operations: Lookup, Getattr, Setattr, Readdir (+ReadDirPlus), Open,
  Read, Write, Flush, Fsync, Create, Mkdir, Unlink, Rmdir, Rename,
  Release. Map each onto the model: cache file present = serve locally;
  miss = hydrate (transport GET; Range if partial - leaf 06); write =
  write to a local staging temp in cacheDir, mark dirty in the index on
  Flush, upload on the prompt-upload path (leaf 07 scheduler calls in;
  this leaf provides the upload entry point as an interface).
- Rename = transport MOVE (atomic server-side) + index update + local
  rename. MOVE enforces If-Match on the destination server-side.
- Directory semantics come from the SERVER shape: webdav collections.
  Mkdir = transport MKCOL. Rmdir = transport DELETE (404 when non-empty
  - surface as ENOTEMPTY).
- Delete-marker visibility: the webdav view suppresses delete-marked
  keys (the listing is the reconciliation view). The client trusts
  PROPFIND listings.
- Reserved-name policy mirrors the server: keys under `.metadata/`,
  `.zfs`, `.uploads` never surface through the mount; the client never
  creates them.
- Dirty tracking: writes go to `<cacheDir>/staging/<uuid>` then rename
  into the cache path on Flush; index `dirty=1` at Flush, `dirty=0`
  only after transport PUT returns 2xx AND the response ETag matches
  what we sent (compute MD5 client-side; the server's ETag IS the MD5).
- Hydration on read miss: GET to transport, stream to the cache file
  (temp + rename), serve reads from local. Partial hydration (Range) is
  a v1 stretch: if implemented, index must record hydrated ranges;
  DEFAULT = full-file hydration (simpler, correct; revisit later).
- Mount lifecycle (locked decisions): mount succeeds even when the
  server is unreachable at startup (queue writes, retry); unmount with
  dirty files = block and flush, or refuse with the reason surfaced via
  IPC `status.state`. SIGTERM flush path must be bounded (a deadline,
  then refuse unmount, keep process alive reporting error state).
- Failure mapping: server 412 on PUT = lost race, the sync engine (leaf
  04) owns conflict resolution - FUSE layer just reports upload-failed
  and keeps the file dirty. Never lose local bytes on any server error.
- e2e: extend case 40 - when FUSE is available (detect fuse kernel
  support; graceful skip otherwise with the documented message), mount
  against the running server: create file via the mount, assert it
  appears server-side (curl GET via S3), write server-side (S3 PUT),
  assert PROPFIND-driven appearance on the mount, read back through
  the mount. Document the manual macFUSE/libfuse mount procedure in
  the case header like case 19 does.

## Constraints

- Do NOT implement sync/conflict logic (leaf 04), quota (leaf 07), or
  the real transport (leaf 06 - code against `internal/transport`
  interface `Get/Put/Mkdir/Delete/Move/Propfind`, stub impl = in-memory
  map for unit tests; leaf 06 fills it).
- FUSE unmount on SIGTERM must not lose dirty data (bounded flush, then
  refuse).
- char content: no cgo.

## Acceptance

1. Unit tests with a stub transport covering: hydrate-on-miss,
   write-through staging + dirty flag, rename local+remote, delete
   flows, reserved-name filtering.
2. e2e case 40 mount section green where FUSE exists (CI linux runners
   lack /dev/fuse - the graceful skip is the CI path; document that in
   the case).
3. `go test -race ./internal/fusefs/` green.
4. Report: chosen FUSE library + why, operation mapping table, and the
   dirty-state transition diagram (states + events, text form).
