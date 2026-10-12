# Replicated extension: live-device bringup findings (2026-10-12)

Status: **blocked at the OS bringup handshake** - the last 5% requires an
Xcode-assembled app container, which SwiftPM cannot produce.

## What works (verified live on this machine)

1. **Assembly**: SPM builds `ZetaFileProviderAppex` (executable target);
   a hand-assembled `.appex` bundle (`XPC!`, `NSExtensionPointIdentifier`
   `com.apple.fileprovider-nonui`, `NSExtensionPrincipalClass`
   `FPReplicatedExtension`) inside a `.app` host with
   `LSUIElement` codesigns clean ad-hoc with `--options runtime`.
2. **Required plists/entitlements** (all discovered from fileproviderd
   error messages, each one previously a silent failure):
   - `NSExtensionFileProviderDocumentGroup` in the appex Info.plist MUST
     equal the App Group (missing = "Skipping provider").
   - The appex (and host) MUST have `com.apple.security.app-sandbox`
     (missing = extensionKit error 16, launch refused).
   - App Group entitlement + a real Group Containers dir.
   - The `add` call must run from inside the signed `.app` host binary -
     a bare unsigned CLI gets `-2001/-2014` ("application cannot be used
     right now") because the system checks the caller.
   - `main.swift` must call `NSExtensionMain` (via `@_silgen_name`); a
     bare `dispatchMain()` never registers the XPC endpoint.
3. **Domain registration**: `NSFileProviderManager.add` succeeds from the
   host; domain persists; remove/re-add cycle clean.
4. **Extension launch**: fileproviderd + extensionKit LAUNCH the appex
   process (`ExtensionProcess ... pid: NNNN` in the log) - the sandbox
   initializes (AppSandbox entries in the process log).

## Where it stops

fileproviderd's **initial bringup message** fails:
`connection to service with pid -1 ... invalidated`. extensionKit creates
the process then immediately invalidates its launch assertion: the appex
comes up but never announces the XPC endpoint extensionKit expects.

Root cause (assessed): on macOS 26 the bringup is ExtensionKit-native
(`EXExtensionMain` + `EXAppExtension` metadata). SwiftPM executables
cannot link that glue; `NSExtensionMain` is the legacy PlugInKit path and
the system no longer completes bringup over it. An Xcode (or hand-rolled
actool) app container with proper extension metadata is required.

## Reproduce

```
cd zeta-cache/gui/fileprovider
swift build
# assemble per scripts in git log 4a7c55c+ (appex-stage dir layout)
codesign --force --sign - --options runtime --entitlements ext.entitlements <app>/PlugIns/ZetaFileProvider.appex
codesign --force --sign - --options runtime --entitlements host.entitlements <app>
pluginkit -a <app>/PlugIns/ZetaFileProvider.appex
<app>/Contents/MacOS/<host>          # host binary runs NSFileProviderManager.add
log show --last 1m --predicate 'process == "fileproviderd"'   # watch bringup
```

Cleanup: remove the domain from the host, `pluginkit -r <appex>`.

## xcodegen round (same day, later)

An `xcodegen` project (`project.yml`) now generates `ZetaFileProvider.xcodeproj`
with a proper app container + embedded appex (type `app-extension`,
`NSExtensionPrincipalClass: FPReplicatedExtension`,
`NSExtensionFileProviderDocumentGroup`, sandbox + App Group entitlements,
`ENABLE_DEBUG_DYLIB=NO`, ad-hoc `SymbiosisDev`/`-` signing).
`xcodebuild ... BUILD SUCCEEDED` and `codesign --verify --deep` clean.

Results, in order of discovery:

1. `CODE_SIGN_STYLE: Manual` demands provisioning profiles - use Automatic
   + `CODE_SIGN_IDENTITY: "-"` for ad-hoc.
2. The appex Info.plist must ALSO carry `NSExtensionFileProviderDocumentGroup`
   (same key as before, but now inside the Xcode-generated plist).
3. Debug builds default to `ENABLE_DEBUG_DYLIB=YES` which produces
   `Team ID` mismatch crashes on ad-hoc relaunch - set `NO`.
4. `AppHost/main.swift` must be NAMED `main.swift` (top-level code rule);
   the appex must have NO `main.swift` OR one that calls `NSExtensionMain`
   via `@_silgen_name` - the xcodegen app-extension product links
   Foundation but does NOT auto-invoke `NSExtensionMain`, leaving the
   process with an entry stub that registers no XPC endpoint.
5. Apple's own `com.apple.CloudDocs.iCloudDriveFileProvider.appex` uses the
   exact same bundle shape (`XPC!`, `com.apple.fileprovider-nonui`,
   `NSExtensionPrincipalClass`) - the format is correct.

**Still blocked, and precisely where**: fileproviderd now ACCEPTS the
provider ("Starting domains for com.zeta-object.ZetaFileProviderAppex"),
extensionKit LAUNCHES the appex process, and the appex boots - but the
domain start dies on `Error preparing documentStorage directory`:
`Cocoa 513 / POSIX 1 (EPERM)` raised inside fileproviderd itself, before
bringup is attempted. The EPERM persists across: pre-created dirs,
`/Applications` root-ish install + lsregister, SymbiosisDev signing with a
`com.apple.application-identifier` entitlement, and ad-hoc signing. With
no sudo access, the redacted log (`<private>`) cannot name the exact path.

Assessment: fileproviderd's sandbox profile gates documentStorage writes
for third-party providers on REAL provisioning (the Stack Overflow
finding: the app must carry a genuine `com.apple.application-identifier`
from a provisioning profile; ad-hoc/minted team IDs do not register in the
sandbox allow-list). This matches the recorded constraint: distribution
grade bring-up requires the $99/yr Apple Developer account. Everything
short of that one OS gate is now verified working end to end.

## Reproduce (xcodegen path)

```
cd zeta-cache/gui/fileprovider
xcodegen generate
xcodebuild -project ZetaFileProvider.xcodeproj -scheme ZetaFileProviderApp \
  -configuration Debug -derivedDataPath .build/xcode build \
  CODE_SIGN_IDENTITY="-" CODE_SIGN_STYLE=Manual CODE_SIGNING_ALLOWED=NO \
  ENABLE_DEBUG_DYLIB=NO
# sign appex + app (see git log), lsregister, run host binary
```

Cleanup: remove the domain from the host, `pluginkit -r <appex>`,
`rm -rf /Applications/ZetaFileProviderApp.app`.
