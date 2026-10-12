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
