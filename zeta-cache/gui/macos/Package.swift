// swift-tools-version:5.9
// zeta-cache menu-bar app (leaf 08). Talks ONLY to the daemon's local
// IPC socket - no network, no credentials.
import PackageDescription

let package = Package(
    name: "zeta-cache-menu",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "ZetaCacheMenu",
            path: "Sources/ZetaCacheMenu"
        ),
        .testTarget(
            name: "ZetaCacheMenuTests",
            dependencies: ["ZetaCacheMenu"],
            path: "Tests/ZetaCacheMenuTests",
            resources: [.copy("Fixtures")]
        ),
    ]
)
