// swift-tools-version:5.9
// zeta-cache FileProvider extension sources (fileprovider-2026-10
// leaf 02). A framework-style package: buildable/testable without an app
// container or pluginkit registration (that manual step is leaf 03).
// Talks ONLY to the daemon's local IPC socket + cache dir - no network,
// no credentials (the locked thin-adapter rule).
import PackageDescription

let package = Package(
    name: "zeta-fileprovider",
    platforms: [.macOS(.v13)],
    targets: [
        // Availability shim: NSFileProviderExtension is marked
        // API_UNAVAILABLE(macos) in the macOS 26 SDK; the shim target
        // redeclares its selector surface so the Swift subclass
        // compiles (see Sources/ZetaFPShim/include/ZetaFPShim.h).
        .target(
            name: "ZetaFPShim",
            path: "Sources/ZetaFPShim",
            publicHeadersPath: "include"
        ),
        .target(
            name: "ZetaFileProvider",
            dependencies: ["ZetaFPShim"],
            path: "Sources/ZetaFileProvider"
        ),
        .testTarget(
            name: "ZetaFileProviderTests",
            dependencies: ["ZetaFileProvider"],
            path: "Tests/ZetaFileProviderTests",
            resources: [.copy("Fixtures")]
        ),
    ]
)
