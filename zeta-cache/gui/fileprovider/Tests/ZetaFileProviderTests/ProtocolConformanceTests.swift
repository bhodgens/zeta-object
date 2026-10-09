// ProtocolConformanceTests.swift - the Swift half of the golden-file
// protocol conformance for the fileprovider package. The SAME JSON
// fixtures the Go encoder's responses were pinned to (zeta-cache/
// internal/ipc/testdata/golden/*.json, copied into Fixtures/) are
// decoded here with the package's Codable types (leaf-02 requirement 2:
// the duplication rule - both sides pinned by one fixture set).

import XCTest
@testable import ZetaFileProvider

final class ProtocolConformanceTests: XCTestCase {

    private func fixtureData(_ name: String) throws -> Data {
        let fileManager = FileManager.default
        // 1. SPM resource bundle layout (next to the test bundle).
        let bundleURL = Bundle(for: ProtocolConformanceTests.self).bundleURL
            .deletingLastPathComponent()
            .appendingPathComponent("zeta-fileprovider_ZetaFileProviderTests.bundle")
            .appendingPathComponent("Fixtures/\(name).json")
        if fileManager.fileExists(atPath: bundleURL.path) {
            return try Data(contentsOf: bundleURL)
        }
        // 2. xcodebuild layout (inside the class bundle).
        if let inBundle = Bundle(for: ProtocolConformanceTests.self)
            .url(forResource: name, withExtension: "json", subdirectory: "Fixtures") {
            return try Data(contentsOf: inBundle)
        }
        // 3. Repo-relative fallback.
        let repoRelative = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .appendingPathComponent("Fixtures/\(name).json")
        if fileManager.fileExists(atPath: repoRelative.path) {
            return try Data(contentsOf: repoRelative)
        }
        throw XCTSkip("fixture \(name).json not found in any known location")
    }

    private func decode(_ name: String, file: StaticString = #filePath, line: UInt = #line) throws -> IPCResponse {
        let data = try fixtureData(name)
        do {
            return try JSONDecoder().decode(IPCResponse.self, from: data)
        } catch {
            XCTFail("\(name): decode failed: \(error)", file: file, line: line)
            throw error
        }
    }

    // MARK: the new leaf-01 fixtures (the fileprovider surface)

    func testEnumerateFixture() throws {
        let resp = try decode("enumerate")
        XCTAssertTrue(resp.ok)
        guard case .enumerate(let data) = resp.extra else {
            return XCTFail("enumerate: extra is not entries")
        }
        XCTAssertEqual(data.entries.count, 2)
        let file = data.entries[0]
        XCTAssertEqual(file.name, "a.txt")
        XCTAssertEqual(file.key, "docs/a.txt")
        XCTAssertEqual(file.size, 5)
        XCTAssertEqual(file.mtime, 1_728_211_200)
        XCTAssertTrue(file.materialized)
        XCTAssertFalse(file.dirty)
        XCTAssertEqual(file.cachePath, "files/docs/a.txt")
        let dir = data.entries[1]
        XCTAssertTrue(dir.isDir)
        XCTAssertEqual(dir.key, "docs/sub/")
        XCTAssertEqual(dir.cachePath, "")
    }

    func testItemFixture() throws {
        let resp = try decode("item")
        guard case .item(let data) = resp.extra else {
            return XCTFail("item: extra is not item")
        }
        XCTAssertEqual(data.item.key, "docs/a.txt")
        XCTAssertEqual(data.item.cachePath, "files/docs/a.txt")
        XCTAssertTrue(data.item.materialized)
    }

    func testDownloadFixture() throws {
        let resp = try decode("download")
        guard case .download(let ack) = resp.extra else {
            return XCTFail("download: extra is not a download ack")
        }
        XCTAssertEqual(ack.path, "docs/a.txt")
        XCTAssertTrue(ack.materialized)
        XCTAssertEqual(ack.cachePath, "files/docs/a.txt")
    }

    func testDehydrateFixture() throws {
        let resp = try decode("dehydrate")
        XCTAssertTrue(resp.ok)
    }

    func testMarkFixture() throws {
        let resp = try decode("mark")
        guard case .mark(let ack) = resp.extra else {
            return XCTFail("mark: extra is not a mark ack")
        }
        XCTAssertEqual(ack.path, "docs/new.txt")
        XCTAssertTrue(ack.dirty)
    }

    func testDeleteFixture() throws {
        let resp = try decode("delete")
        guard case .delete(let ack) = resp.extra else {
            return XCTFail("delete: extra is not a delete ack")
        }
        XCTAssertEqual(ack.path, "docs/old.txt")
    }

    func testMoveFixture() throws {
        let resp = try decode("move")
        guard case .move(let ack) = resp.extra else {
            return XCTFail("move: extra is not a move ack")
        }
        XCTAssertEqual(ack.from, "docs/old.txt")
        XCTAssertEqual(ack.to, "docs/new.txt")
    }

    func testErrorFixtures() throws {
        let dl = try decode("download-missing-path")
        XCTAssertFalse(dl.ok)
        XCTAssertEqual(dl.error, "download requires path")
        let mv = try decode("move-missing-to")
        XCTAssertFalse(mv.ok)
        XCTAssertEqual(mv.error, "move requires path and to fields")
        let unknown = try decode("unknown-type")
        XCTAssertFalse(unknown.ok)
        XCTAssertEqual(unknown.error, "unknown request type")
        let badVersion = try decode("bad-version")
        XCTAssertFalse(badVersion.ok)
    }

    // MARK: the older shared fixtures (the copied leaf-08 types decode
    // them too - the duplication rule's regression net)

    func testStatusFixture() throws {
        let resp = try decode("status")
        XCTAssertTrue(resp.ok)
        XCTAssertEqual(resp.v, 1)
        let data = try XCTUnwrap(resp.data)
        XCTAssertEqual(data.state, "idle")
        XCTAssertEqual(data.server, "https://cache.example")
        XCTAssertEqual(data.bucket, "bkt")
        XCTAssertNil(data.lastSync?.unixSeconds)
    }

    func testPinsAndTombstonesFixtures() throws {
        let pins = try decode("pins")
        guard case .pins(let list) = pins.extra else {
            return XCTFail("pins: extra is not pins")
        }
        XCTAssertEqual(list, ["docs/keep.txt", "photos/raws"])
        let tombs = try decode("tombstones")
        guard case .tombstones(let rows) = tombs.extra else {
            return XCTFail("tombstones: extra is not tombstones")
        }
        XCTAssertEqual(rows.first?.path, "notes/old.txt")
    }

    func testConflictsFixtures() throws {
        let resp = try decode("conflicts-list")
        guard case .conflicts(let conflicts) = resp.extra else {
            return XCTFail("conflicts-list: extra is not conflicts")
        }
        XCTAssertEqual(conflicts.count, 2)
        XCTAssertEqual(conflicts[0].kind, "conflict-copy")
        let resolve = try decode("conflicts-resolve")
        guard case .resolve(let ack) = resolve.extra else {
            return XCTFail("conflicts-resolve: extra is not resolve")
        }
        XCTAssertEqual(ack.mode, "keep-local")
        XCTAssertTrue(ack.copyRemoved)
    }
}
