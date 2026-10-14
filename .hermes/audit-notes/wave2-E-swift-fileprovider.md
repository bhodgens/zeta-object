# Wave 2-E — fix-efficacy + defect audit: Swift FileProvider extension + macOS gui (HEAD 7f4a544)

Read-only. No tracked file modified (`git status --porcelain` = only this notes dir, untracked).
Assigned findings: H6, H7, M27, L41–L45, F1, F2, F3. Assigned fix commits: f03eef7, 1d3b112.

## Gates (exact commands)

| command | result |
|---|---|
| `cd zeta-cache/gui/fileprovider && swift build` | **Build complete! (0.97s)**; 1 warning: `Sources/ZetaFileProvider/IPC.swift:117:16: warning: value 'dl' was defined but never used` |
| `cd zeta-cache/gui/fileprovider && swift test` | **Executed 39 tests, with 0 failures** |
| `cd zeta-cache/gui/macos && swift test` | **Executed 11 tests, with 0 failures** |
| `cd zeta-cache/gui/fileprovider && xcodebuild -project ZetaFileProvider.xcodeproj -scheme ZetaFileProviderApp -configuration Debug -destination 'platform=macOS' CODE_SIGNING_ALLOWED=NO build` | **BUILD FAILED** — `error: Build input file cannot be found: '.../gui/fileprovider/Appex/main.swift'`. Cause: `ZetaFileProvider.xcodeproj/` is gitignored (`.gitignore:3 *.xcodeproj`, "xcodegen output") and was generated while `Appex/main.swift` still existed (pbxproj has TWO `main.swift` PBXFileReference entries, lines 47-48, one under the Appex group at :89). NOT a repo defect — the tracked spec (`project.yml` `sources: [Appex]` + `Appex/AppexSupport.swift`) contains no main.swift. A regeneration + rebuild was attempted (`xcodegen` is present at /opt/homebrew/bin/xcodegen) but the operator **denied** the command, twice, so the Xcode link path stays unverified. |
| `cd zeta-cache && CGO_ENABLED=0 go test -count=1 -run 'Golden\|Pins' ./internal/ipc/` | `ok ... 0.266s` |

## The key seam (verified at HEAD)

Byte destination of a create/edit: `FileProviderExtension.cacheURL(ofKey:)` →
`<cacheDir>/files/<key>` (`FileProviderExtension.swift:153-155`), reachable via
`cacheURLCreatingParents(ofKey:)` (`:159-164`), used by Replicated create
(`FileProviderReplicatedExtension.swift:95`) and modify (`:152`).
Daemon mark: `guiHandler.cacheFilePath` = `<cacheDir>/files/<key>`
(`fileprovider_handler.go:26-27`), and `Mark` `os.Stat`s exactly that path before
writing the dirty row (`fileprovider_handler.go:206-227`). **Seam MATCHES.**
Daemon cacheDir default = `~/Library/Application Support/zeta-cache`
(`internal/config/config.go:65-67`), equal to the extension's unsandboxed
fallback (`FileProviderExtension.swift:57-58`). IPC wire shape decodes on both
sides for every leaf-01 payload (enumerate/item/download/mark/delete/move) —
golden fixtures at `internal/ipc/testdata/golden/*.json`, asserted on the Go side
by `golden_test.go` and decoded on the Swift side by
`Tests/ZetaFileProviderTests/ProtocolConformanceTests.swift:50-120`.

## Findings (detail)

### 1. HIGH — every DIRECTORY item reports itself as its own parent
`Sources/ZetaFileProvider/FileProviderItem.swift:57-60` (`parentDirKey`), used at
`FileProviderEnumerator.swift:63` and `:92` and `FileProviderExtension.swift:207`;
and `Sources/ZetaFileProviderReplicated/FileProviderReplicatedExtension.swift:188-192`
(`parentKeyOf`), used at `:54`, `:74`, `:105`, `:161`.
`parentDirKey` slices `key[...idx]` INCLUSIVE of the separator; `parentKeyOf`
slices `key[..<slash]` EXCLUSIVE — neither drops a dir key's own trailing slash
first. Verified semantics with the identical expressions compiled by swift:

    key=docs/a.txt      parentDirKey=docs/      parentKeyOf=docs
    key=docs/sub/       parentDirKey=docs/sub/  parentKeyOf=docs/sub     <-- SELF
    key=sub/            parentDirKey=sub/       parentKeyOf=sub          <-- SELF
    key=docs/sub/deep/  parentDirKey=docs/sub/deep/  parentKeyOf=docs/sub/deep  <-- SELF

The daemon emits dir keys WITH the trailing slash
(`fileprovider_handler.go:64` `IsDir: strings.HasSuffix(path,"/")`, `:105`
`Key: dirKey + sub`, golden `enumerate.json` dir entry `"key":"docs/sub/"`).
So `parentItemIdentifier` == `itemIdentifier` for every folder. The SDK rejects
a self-parented child, so nested folders never surface in Finder — the same
user-visible symptom class H6/F1 fixed for files, still open for dirs.
Trigger: enumerate any container holding a subdirectory.
Test that hides it: `FileProviderItemMappingTests.swift:50-59` constructs the dir
item with an EXPLICIT `parentKey: "docs/"` (the correct value) instead of deriving
it from `entry.key` the way all production call sites do.
Fix: drop one trailing "/" before slicing in BOTH helpers
(`let k = key.hasSuffix("/") ? String(key.dropLast()) : key`), and derive the
parent in the dir test from `entry.key`.

### 2. MEDIUM — `modifyItem` ignores a reparent that keeps the filename
`FileProviderReplicatedExtension.swift:141-150`. Only
`changedFields.contains(.filename)` is handled; the SDK signals a move to a
different folder via `changedFields` containing `.parentItemIdentifier`. The
new key is computed FROM `item.parentItemIdentifier` (`:142-143`), so a reparent
alone builds nothing new, issues no IPC move, and returns the item under its OLD
key/old parent (`:161`).
Trigger: drag `a.txt` from folder A to folder B in Finder (name unchanged).
Effect: no `move` IPC, remote key unchanged, item keeps parent A — the file
appears not to move.
Fix: also branch on `.parentItemIdentifier` and compute
`newKey = parentKeyForChild(item.parentItemIdentifier.rawValue) + "/" + item.filename`
whenever either flag is set.

### 3. MEDIUM — deleting a folder is a file-shaped delete against a dir key
`FileProviderReplicatedExtension.swift:170-183` (and the legacy
`FileProviderExtension.swift:275-282`) send `identifier.rawValue`, i.e. `"docs/"`
for a folder. The wire layer `normalizePath`s the trailing slash away
(`internal/ipc/fileprovider.go:127-130`, applied at `:193`), then `Delete`
(`fileprovider_handler.go:229-238`) does `os.Remove(<cacheDir>/files/docs)` and
`db.DeletePath("docs")`. The mirror dir was created by the F2 dir branch
(`FileProviderReplicatedExtension.swift:118-119`) and a folder with any locally
created child is NON-empty, so `os.Remove` fails ENOTEMPTY → the delete errors
out; when the mirror dir is absent the tombstone is still the FILE form `docs`,
not the dir key `docs/`, and dirs have no daemon row at all (F2's own rationale).
Trigger: delete a folder in Finder.
Effect: folder delete fails (non-empty mirror) or tombstones a non-existent
file-shaped key; the remote collection's fate is unverified (a webdav DELETE of
a collection is a recursive delete, H10).
Fix: route dir identifiers to a dir-aware path (tombstone the dir key and delete
the remote collection) or refuse with a clear `.cannotSynchronize`.

### 4. LOW — `createItem` overwrites an existing destination with no collision signal
`FileProviderReplicatedExtension.swift:92-100` (legacy `FileProviderExtension.swift:257-265`):
`if fileExists { removeItem } ; copyItem ; mark`. No index lookup and no
`NSFileProviderError.filenameCollision`, so a create for a name that already
exists silently replaces the object's bytes and marks them dirty.
Trigger: a create whose name already exists (replayed create, or a same-name
object already in the cache/remote).
Effect: silent replacement of the existing object (no conflict copy).
Fix: check the key first (IPC `item` or a local stat) and return
`.filenameCollision` unless `options.contains(.mayAlreadyExist)`.

### 5. LOW — the dehydrate payload has no Swift-side contract pin
`Tests/ZetaFileProviderTests/ProtocolConformanceTests.swift:91-94`
(`testDehydrateFixture`) asserts only `resp.ok`. The fileprovider `ExtraPayload`
has no `.dehydrate` case — a bare-path ack decodes as `.delete`
(`IPC.swift:134-143`) — so the dehydrate shape (`{"path"}`,
`internal/ipc/fileprovider.go:63-66` / golden `dehydrate.json`) is unpinned: a
change to the Go payload keeps the test green. The test CAN fail on a decode
error, but it asserts nothing about the contract it names.
Fix: give dehydrate a distinct `ExtraPayload` case or assert the path value.

### 6. LOW — the download branch probes a Bool and leaves it unused
`Sources/ZetaFileProvider/IPC.swift:117-123`: `if let dl = try c.decodeIfPresent(Bool.self, forKey: .materialized)`
then re-decodes the container as `DownloadAck`. `dl` is never read (the only
swift-build warning), and the branch keys on presence alone, so any payload that
carries `"materialized"` without `"cachePath"` throws `dataCorrupted` out of the
whole response even though `ok:true`.
Fix: use `!= nil` and require `cachePath` in the same guard (or decode
`DownloadAck` directly and validate).

### 7. LOW — the Swift decoders still cannot represent `"pins":null` (the other half of M27)
Go is fixed (`internal/ipc/ipc.go:79-84` `newPinData` normalises nil→`[]`, used
at `:372` and `:383`; `m27_pins_test.go` pins the exact bytes `{"pins":[]}`).
But BOTH Swift decoders
(`gui/macos/Sources/ZetaCacheMenu/IPC.swift:80`, `gui/fileprovider/.../IPC.swift:97`)
resolve `decodeIfPresent([String].self, forKey: .pins)` to nil for a JSON null and
then fall through to `throw DecodingError.dataCorrupted("unrecognized extra
payload")`. No Swift test feeds the null shape, so the decoder half of M27 is
ungated: a future emitter (or a regenerated fixture) that writes null breaks the
menu app and the extension without a red test.
Fix: one Swift test per package feeding `{"v":1,"ok":true,"extra":{"pins":null}}`
and asserting `.pins([])` (or explicitly swallow null).

### 8. LOW — L45: sandboxed appex cacheDir is a tree the daemon never writes
`Sources/ZetaFileProvider/FileProviderExtension.swift:43-59`: under the app-group
entitlement `defaultCacheDir()` returns
`containerURL(forSecurityApplicationGroupIdentifier:"group.com.zeta-object.zeta-cache")`,
but the daemon's cacheDir is `~/Library/Application Support/zeta-cache`
(`config.go:65-67`) and nothing points it at the group container. The code
documents this as `TODO(appex-sandbox)` (`:46-49`). Also note the socket default
uses `NSTemporaryDirectory()` (`:66-67`), which in a sandbox is the app's own
container tmp, not the daemon's `$TMPDIR` runtime dir — the same class.
Effect (if the appex is run as shipped): mark stats a different tree → every
create/edit fails at mark, i.e. the H6 symptom returns in the sandboxed config.
Not closable in Swift; needs the daemon-side change.

## Closed checks

- **H6 = CLOSED.** `cacheURL(ofKey:)` returns `files/<key>`
  (`FileProviderExtension.swift:153-155`); the daemon's mark stats
  `<cacheDir>/files/<key>` (`fileprovider_handler.go:26-27`, `:206-210`);
  `ReplicatedExtensionTests.swift:111-115` pins the byte path AND asserts nothing
  strands at the old `cacheDir/<key>`. Real contract, not ok:true.
- **H7 = CLOSED.** `key = newKey` before the content copy/mark
  (`FileProviderReplicatedExtension.swift:149`, used at `:152-157`);
  `ReplicatedExtensionTests.swift:213-247` pins the new key on the data plane and
  asserts the old key keeps its old bytes.
- **M27 = CLOSED** (Go emitter half). `newPinData` (`ipc.go:79-84`) + call sites
  `:372`/`:383`; `m27_pins_test.go` byte-pins `{"pins":[]}`. Swift half unpinned
  (finding 7).
- **L41 = CLOSED.** `FileProviderExtension.swift:66-67` uses
  `NSTemporaryDirectory()/zeta-cache.ipc`; the menu app is identical
  (`ZetaCacheMenuApp.swift:45`); the daemon default runtime dir is
  `os.TempDir()` on darwin (`config.go:78-86`). (Sandbox caveat: finding 8.)
- **L42 = CLOSED.** `FileProviderItem.swift:182-205` splits
  connectivityMarkers → `.serverUnreachable` from refusalMarkers/unrecognised →
  `.cannotSynchronize`; `FileProviderItemMappingTests.swift:82-118` asserts the
  real `(domain, code)` pairs for the daemon's actual refusal strings.
- **L43 = CLOSED.** `FileProviderEnumerator.swift:69-70` finishes with a real
  deterministic anchor (FNV-1a over sorted `key|mtime|size|isDir` rows, `:32-45`)
  and `:81-96` implements `enumerateChanges`. The content-hash limitation is
  documented; `ReplicatedExtensionTests.swift:375-390` pins
  determinism/order-independence/sensitivity.
- **L44 = CLOSED (source level).** `git ls-files Appex` = AppexSupport.swift +
  Info.plist + entitlements only; the sole explicit `NSExtensionMain` is
  `Sources/ZetaFileProviderAppex/main.swift` (a distinct SwiftPM executable
  target, not in the Xcode appex target's `sources: [Appex]`), and
  `Appex/AppexSupport.swift` records the rationale. The Xcode link path itself
  stays UNVERIFIED (see Gates) because the on-disk generated project is stale and
  regeneration was denied.
- **L45 = OPEN** (documented: `FileProviderExtension.swift:46-49` TODO). See
  finding 8.
- **F1 (1d3b112) = CLOSED.** `parentKeyForChild` strips ONE trailing slash and
  maps the root/working-set sentinels (`FileProviderReplicatedExtension.swift:202-208`),
  applied at `:90` (create) and `:142` (rename). The tests now use the REAL
  dir-identifier shape (`ReplicatedExtensionTests.swift:138-139` asserts
  `"fp/"`) and pin the mark path to the single-slash form (`:152-153`) and the
  move pair (`:284-285`) — real contract.
- **F2 (1d3b112) = CLOSED.** The dir branch performs NO mark
  (`FileProviderReplicatedExtension.swift:106-125`); the test asserts zero mark
  requests and the golden dir-entry shape (`ReplicatedExtensionTests.swift:165-185`).
- **F3 (1d3b112) = CLOSED.** README rewritten to the SwiftPM appex entry
  (`zeta-cache/README.md:254-274`), `AppexSupport.swift` documents the Xcode-linked
  entry. Minor residual drift: `README.md:248` still says "31 tests" (39 now).

## Test-quality notes

- Real-contract (can fail): H6/H7/F1/F2 tests (data plane + IPC request pins via
  the new `StubIPCServer.requests` recorder, `StubDaemon.swift`), L42 mapping
  tests, L43 anchor tests, the Go side of M27.
- Weak (cannot fail for the contract they name): `testDehydrateFixture`
  (finding 5); the dir-parent contract has NO test at all (finding 1 — the only
  dir-mapping test hand-feeds the correct parent).
- The report's "asserts nothing" entries for `ReplicatedExtensionTests.swift:166-168`
  and `ProtocolConformanceTests.swift:91-94` are now respectively a real
  assertion block (dir create) and the still-weak dehydrate assertion above.
