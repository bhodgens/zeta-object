# macOS FileProvider extension client (iCloud-grade virtual files)

Status: FUTURE work. The near-term client cache (zeta-cache) is FUSE-only on
Linux and macOS. This issue records the eventual native macOS path so the
decision and its requirements are not lost.

## Background

Goal: use zeta-object as backing storage for a networked $HOME. The client
device keeps a transparent local cache; infrequently used files live only on
the server. Eviction policy is client-owned (size threshold, size+age, LRU).
The server stays a dumb gateway per the repo charter: no server-owned
per-client state.

The ownCloud desktop client route was evaluated and rejected: its Virtual
Files suffix mode is deprecated on macOS (client 6.x, 2025) and Linux has no
OS VFS API. Details live in the client-cache design discussion (zeta-cache,
FUSE daemon, Go, go-fuse on Linux, macFUSE on macOS for v1).

## What the FileProvider path gives

Apple's FileProvider framework (the API iCloud Drive uses) gives:

- Placeholder files managed by macOS itself: no macFUSE install, no
  `.owncloud`-style suffix files, full Finder integration.
- OS-managed on-demand download and dehydration callbacks, which map onto
  the same eviction policy engine the FUSE daemon uses.

## What it requires

- A macOS app plus a FileProvider extension in Swift (the extension cannot
  be Go; FileProvider extensions are Swift/Objective-C).
- Code signing and notarization for distribution outside the App Store.
- The sync/protocol layer can reuse the zeta-cache client protocol
  (S3 or WebDAV) - no server change required. Server improvements that help
  both clients: Range GET (206) on webdav/owncloud, If-None-Match
  revalidation on GET.

## Tracking

Filed as zeta-object issue #14
(https://github.com/bhodgens/zeta-object/issues/14). The in-repo copy is the
durable record.

## Open questions

- Swift/Go boundary: full Swift reimplementation of the sync layer, versus
  the Go daemon running as a login service with the extension as a thin
  adapter. The extension has memory and runtime limits that make a thin
  adapter attractive but complicate lifecycle.
- Minimum macOS target: FileProvider exists since macOS 11; placeholder
  behaviors are dependable from macOS 13+.
- Whether the eviction policy engine can share one implementation with the
  FUSE daemon (extracted config + policy core) or forks per platform.
