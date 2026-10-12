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
    products: [
        // Consumed by the Xcode app container (project.yml): the appex
        // target links the Replicated extension sources via these.
        .library(name: "ZetaFileProvider", targets: ["ZetaFileProvider"]),
        .library(name: "ZetaFileProviderReplicated", targets: ["ZetaFileProviderReplicated"]),
    ],
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
        // The MODERN Replicated extension (issue #14 remainder):
        // NSFileProviderReplicatedExtension, macOS 11+. No shim needed -
        // the protocol is available on macOS.
        .target(
            name: "ZetaFileProviderReplicated",
            dependencies: ["ZetaFileProvider"],
            path: "Sources/ZetaFileProviderReplicated"
        ),
        // The .appex executable: the system launches this binary and
        // instantiates the principal class named in the appex Info.plist
        // (NSExtensionPrincipalClass). main.swift just keeps the process
        // alive after the class registers.
        .executableTarget(
            name: "ZetaFileProviderAppex",
            dependencies: ["ZetaFileProviderReplicated"],
            path: "Sources/ZetaFileProviderAppex"
        ),
        .testTarget(
            name: "ZetaFileProviderTests",
            dependencies: ["ZetaFileProvider", "ZetaFileProviderReplicated"],
            path: "Tests/ZetaFileProviderTests",
            resources: [.copy("Fixtures")]
        ),
    ]
)
