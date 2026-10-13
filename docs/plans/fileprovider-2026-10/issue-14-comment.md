Shipped state (fileprovider-2026-10 plan tree, leaves 01-03):

**The thin-adapter implementation has landed.** The open Swift/Go
boundary question is resolved in favor of the thin adapter: the Go daemon
(zeta-cache) stays the single network-attached, credential-holding
process; the FileProvider extension speaks only the daemon's local IPC
protocol and reads/writes the daemon's cache dir. Landmark commit:
5dd16f3 (leaves 01+02: IPC v2 surface + Swift adapter).

**What works:**

- Enumerate / item / download (materialize-on-open) / dehydrate /
  mark / delete / move - all through the daemon's IPC socket; file
  bytes never cross the socket (the extension opens cache files
  directly, same user).
- The extension is credential-free and makes no network calls; sync,
  cache, quota, and eviction policy stay daemon-owned (client-cache-2026-10
  leaves 01-07, already complete).
- Coverage: 23 Swift conformance tests (golden-fixture protocol
  conformance + item-mapping tests in `gui/fileprovider/Tests`), and
  43 case-40 IPC asserts (section 40g of
  `scripts/e2e/cases/40-zeta-cache.sh`, IPC-only, green on CI).

**Manual gates (documented, not automated):**

- Build: `cd zeta-cache/gui/fileprovider && swift build` (buildable
  without an app container; `swift test` for conformance).
- Registration: `pluginkit -a <appex>` - a manual step; verify with
  `fileproviderctl status` / Finder's sidebar. Documented in
  `zeta-cache/README.md`.
- Distribution to any machine other than the build machine REQUIRES
  code signing (Developer ID Application identity) + notarization
  (notarytool submit + stapler). This is the explicit distribution
  gate and is future work - not automated, no App Store target yet.

**Known limitations:** classic `NSFileProviderExtension` API, not the
modern Replicated extension (which needs an app container); server
locks are exclusive-only; eviction is daemon-owned (Finder's "Remove
Download" maps to IPC dehydrate).

**Keeping this issue OPEN:** the full Replicated-extension + App Store
path (app target, domain management via `NSFileProviderManager`, App
Store distribution) remains future work and is the reason this issue
stays open. The thin-adapter daemon + classic-extension path above is
done and documented.
