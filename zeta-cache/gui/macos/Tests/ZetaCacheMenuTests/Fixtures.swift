// FixtureTests.swift helper - locate the SPM resources bundle that
// carries the Fixtures/ directory. SPM places target resources in a
// separate `<package>_<target>.bundle` NEXT TO the xctest bundle, and
// Bundle(for:) does not see it, so search the filesystem directly.
import XCTest
@testable import ZetaCacheMenu

enum Fixtures {
    static func url(_ name: String) throws -> URL {
        let fileManager = FileManager.default
        // 1. Next to the test bundle (SPM layout).
        if let testBundleURL = Bundle(for: ProtocolConformanceTests.self).bundleURL
            .deletingLastPathComponent()
            .appendingPathComponent("zeta-cache-menu_ZetaCacheMenuTests.bundle")
            .appendingPathComponent("Fixtures/\(name).json") as URL?,
            fileManager.fileExists(atPath: testBundleURL.path) {
            return testBundleURL
        }
        // 2. Inside the class bundle (xcodebuild layout).
        if let inBundle = Bundle(for: ProtocolConformanceTests.self)
            .url(forResource: name, withExtension: "json", subdirectory: "Fixtures") {
            return inBundle
        }
        // 3. Repo-relative fallback (running from the package root).
        let repoRelative = URL(fileURLWithPath: #filePath) // .../Tests/...swift
            .deletingLastPathComponent()                    // Tests/ZetaCacheMenuTests
            .appendingPathComponent("Fixtures/\(name).json")
        if fileManager.fileExists(atPath: repoRelative.path) {
            return repoRelative
        }
        throw XCTSkip("fixture \(name).json not found in any known location")
    }

    static func data(_ name: String) throws -> Data {
        try Data(contentsOf: try url(name))
    }
}
