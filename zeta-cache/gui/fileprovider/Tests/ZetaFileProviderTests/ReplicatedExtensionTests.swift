// ReplicatedExtensionTests.swift - conformance tests for the MODERN
// Replicated extension (Sources/ZetaFileProviderReplicated), driven
// against the same in-process StubIPCServer the leaf-02 tests use. The
// Replicated protocol's completion-handler surface is exercised
// end-to-end: item lookup, content fetch, create/modify/delete, and
// the copy-through-cache write path.

import FileProvider
import XCTest
@testable import ZetaFileProvider
@testable import ZetaFileProviderReplicated

final class ReplicatedExtensionTests: XCTestCase {

    private var server: StubIPCServer!
    private var cacheDir: URL!
    private var mount: FileProviderMount!
    private var ext: ReplicatedExtension!

    override func setUpWithError() throws {
        // Short socket path (sockaddr_un.sun_path is ~104 bytes).
        let dir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("zfpR-\\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        cacheDir = dir
        server = try StubIPCServer(path: dir.appendingPathComponent("z.ipc").path)
        mount = FileProviderMount(socketPath: server.path, cacheDir: dir)
        ext = ReplicatedExtension(
            domain: NSFileProviderDomain(identifier: NSFileProviderDomainIdentifier("test"), displayName: "test"),
            mount: mount)
    }

    override func tearDown() {
        server.stop()
        try? FileManager.default.removeItem(at: cacheDir)
    }

    private let itemJSON =
        ##"{"v":1,"ok":true,"extra":{"item":{"name":"doc.txt","key":"fp/doc.txt","isDir":false,"size":11,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"files/fp/doc.txt"}}}"##

    // MARK: - item(for:)

    func testItemForIdentifierResolvesThroughIPC() throws {
        server.respond(to: "item", with: itemJSON)

        let exp = expectation(description: "item")
        _ = ext.item(for: NSFileProviderItemIdentifier("fp/doc.txt"), request: NSFileProviderRequest()) { item, error in
            XCTAssertNil(error)
            XCTAssertEqual(item?.filename, "doc.txt")
            XCTAssertEqual(item?.itemIdentifier.rawValue, "fp/doc.txt")
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    func testItemForMissingKeySurfacesError() throws {
        server.respond(to: "item", with: #"{"v":1,"ok":false,"error":"item not found"}"#)

        let exp = expectation(description: "item-miss")
        _ = ext.item(for: NSFileProviderItemIdentifier("nope"), request: NSFileProviderRequest()) { item, error in
            XCTAssertNil(item)
            XCTAssertNotNil(error)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    // MARK: - fetchContents

    func testFetchContentsReturnsCacheFileURL() throws {
        // The daemon hydrates into its cache dir; the extension must hand
        // back the on-disk URL (data via the FILE, the Replicated contract).
        let cacheFile = cacheDir.appendingPathComponent("files/fp/doc.txt")
        try FileManager.default.createDirectory(at: cacheFile.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("remote body".utf8).write(to: cacheFile)

        server.respond(
            to: "download",
            with: #"{"v":1,"ok":true,"extra":{"path":"fp/doc.txt","materialized":true,"cachePath":"files/fp/doc.txt"}}"#)
        server.respond(to: "item", with: itemJSON)

        let exp = expectation(description: "fetch")
        _ = ext.fetchContents(for: NSFileProviderItemIdentifier("fp/doc.txt"), version: nil, request: NSFileProviderRequest()) { url, item, error in
            XCTAssertNil(error)
            XCTAssertEqual(url, self.cacheDir.appendingPathComponent("files/fp/doc.txt"))
            XCTAssertNotNil(item)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    // MARK: - createItem

    func testCreateItemWithContentsCopiesThroughCacheAndMarks() throws {
        let localFile = cacheDir.appendingPathComponent("upload-src.txt")
        try Data("new bytes".utf8).write(to: localFile)
        server.respond(to: "mark", with: #"{"v":1,"ok":true,"extra":{"path":"doc.txt","dirty":true}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"doc.txt","key":"doc.txt","isDir":false,"size":9,"mtime":1728211200,"materialized":true,"dirty":true,"cachePath":"doc.txt"}}}"##)

        let template = ReplicatedTemplate(filename: "doc.txt", parent: .rootContainer)
        let exp = expectation(description: "create")
        _ = ext.createItem(basedOn: template, fields: [.contents], contents: localFile, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(item?.filename, "doc.txt")
            // The bytes must have landed in the daemon's cache dir before
            // mark (the copy-through-cache contract).
            let cached = try? Data(contentsOf: self.cacheDir.appendingPathComponent("doc.txt"))
            XCTAssertEqual(cached, Data("new bytes".utf8))
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    func testCreateItemWithoutContentsCreatesDirectory() throws {
        server.respond(to: "mark", with: #"{"v":1,"ok":true,"extra":{"path":"docs/","dirty":false}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"docs","key":"docs","isDir":true,"size":0,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"docs"}}}"##)

        let template = ReplicatedTemplate(filename: "docs", parent: .rootContainer)
        let exp = expectation(description: "mkdir")
        _ = ext.createItem(basedOn: template, fields: [], contents: nil, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            var isDir: ObjCBool = false
            XCTAssertTrue(FileManager.default.fileExists(atPath: self.cacheDir.appendingPathComponent("docs").path, isDirectory: &isDir))
            XCTAssertTrue(isDir.boolValue)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    // MARK: - modifyItem

    func testModifyItemFilenameMoveRenamesThroughIPC() throws {
        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"doc.txt","to":"renamed.txt"}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"renamed.txt","key":"renamed.txt","isDir":false,"size":11,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"renamed.txt"}}}"##)

        let src = ReplicatedTemplate(filename: "doc.txt", parent: .rootContainer)
        let item = FileProviderItem(entry: entry(name: "renamed.txt", key: "renamed.txt"), parentKey: "")
        let exp = expectation(description: "move")
        _ = ext.modifyItem(item, baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)), changedFields: [.filename], contents: nil, options: [], request: NSFileProviderRequest()) { modified, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(modified?.filename, "renamed.txt")
            exp.fulfill()
        }
        _ = src // template unused on modify
        wait(for: [exp], timeout: 5)
    }

    // MARK: - deleteItem

    func testDeleteItemForwardsThroughIPC() throws {
        server.respond(to: "delete", with: #"{"v":1,"ok":true,"extra":{"path":"doc.txt"}}"#)

        let exp = expectation(description: "delete")
        _ = ext.deleteItem(identifier: NSFileProviderItemIdentifier("doc.txt"), baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)), options: [], request: NSFileProviderRequest()) { error in
            XCTAssertNil(error)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    // MARK: - invalidate

    func testInvalidateIsANoOpThatDoesNotThrow() {
        ext.invalidate()
    }
}

// MARK: - test doubles

/// Minimal NSFileProviderItem for templates/parents in the tests above.
final class ReplicatedTemplate: NSObject, NSFileProviderItem {
    let filename: String
    let parentItemIdentifier: NSFileProviderItemIdentifier

    init(filename: String, parent: NSFileProviderItemIdentifier) {
        self.filename = filename
        self.parentItemIdentifier = parent
        super.init()
    }

    var itemIdentifier: NSFileProviderItemIdentifier { NSFileProviderItemIdentifier(filename) }
}

private func entry(name: String, key: String) -> FileProviderItemEntry {
    // Decode a full entry from JSON (the wire shape the daemon sends).
    let json = "{\"name\":\"" + name + "\",\"key\":\"" + key
        + "\",\"isDir\":false,\"size\":11,\"mtime\":1728211200,\"materialized\":true,\"dirty\":false,\"cachePath\":\"" + key + "\"}"
    return try! JSONDecoder().decode(FileProviderItemEntry.self, from: Data(json.utf8))
}
