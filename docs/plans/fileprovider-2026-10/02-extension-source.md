# Leaf 02 - FileProvider extension sources (thin Swift adapter)

## Goal

gui/fileprovider/: the Swift sources for an NSFileProviderExtension that
bridges Finder to the zeta-cache daemon - enumerate via IPC, data via the
local cache files (leaf 01's contract), mutations via IPC ops.

## Requirements

1. SPM package gui/fileprovider with a framework-style target
   (buildable/testable without an app container; the pluginkit
   registration is leaf 03's documented manual step):
   - FileProviderExtension: NSFileProviderExtension subclass -
     enumeratorForMaterializedItems / enumeratorForPendingPopulation,
     storage service URL rules (the extension's domain folder),
     importDoesUserVisibleSemanticRename semantics documented.
   - FileProviderEnumerator: enumerate the root and dirs via IPC
     `enumerate` (leaf 01), item identity = the server key, fill
     NSFileProviderItem fields (size, mtime, contentPolicy
     .downloadsWhenOpened for non-materialized).
   - FileProviderItem: NSFileProviderItem conformance mapped from the
     IPC entry (materialized -> contentPolicy .always; not ->
     .downloadsWhenOpened; dirty flag -> document the semantics).
   - Mutations: create/delete/rename via IPC ops (delete/move map to
     the daemon's delete/move; create = write to cacheDir/files/<key>
     directly - same user - then IPC to mark dirty/trigger upload).
   - Download trigger: on .downloadsWhenOpened item open, IPC `download`
     (leaf 01's blocking-hydrate), then serve bytes from cacheDir.
   - Dehydration: NSFileProviderDehydratableItem conformance calling
     IPC `dehydrate`.
2. Reuse leaf 08's Codable JSON types where shapes match (import the
   gui/macos IPC.swift by copying the Codable structs - SPM cannot
   cross-import the two packages; document the duplication rule: the
   golden fixtures from leaf 08 pin BOTH sides, add fixtures for the
   new types to the same pattern).
3. Unit tests (XCTest): item mapping (IPC entry -> NSFileProviderItem
   fields), enumerator paging, download-trigger happy path with a stub
   IPC server, error mapping (ENOENT -> .noSuchItem, EIO -> .notAKnown
   error surfaced).
4. swift build + swift test green on this machine (macOS 26.7,
   Swift 6.2.3). FileProvider framework is available; the extension
   classes can be compiled without registering with pluginkit.

## Constraints

- No network in the extension (locked): every operation is IPC or local
  file.
- No credentials in the extension (locked).
- The extension NEVER writes outside the daemon's cache dir + its own
  domain folder.

## Acceptance

1. swift build + swift test green (unit tests above).
2. Report: the item-mapping table (IPC field -> NSFileProviderItem
   property), the operation mapping table (Finder action -> IPC/local
   op), and the documented signing/notarization gate (leaf 03).
