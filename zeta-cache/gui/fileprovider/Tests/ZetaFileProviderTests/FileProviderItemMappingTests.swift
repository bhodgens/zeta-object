// FileProviderItemMappingTests.swift - the IPC entry -> NSFileProviderItem
// mapping tests (fileprovider-2026-10 leaf 02 acceptance 3), plus the
// error mapping and the download-trigger happy path over a stub IPC
// server.

import FileProvider
import XCTest
@testable import ZetaFileProvider

final class FileProviderItemMappingTests: XCTestCase {

    private func entry(
        name: String = "a.txt", key: String = "docs/a.txt", isDir: Bool = false,
        size: Int64 = 5, mtime: Int64 = 1_728_211_200, materialized: Bool = true,
        dirty: Bool = false, cachePath: String = "files/docs/a.txt"
    ) -> FileProviderItemEntry {
        FileProviderItemEntry(
            name: name, key: key, isDir: isDir, size: size, mtime: mtime,
            materialized: materialized, dirty: dirty, cachePath: cachePath)
    }

    func testMaterializedFileMapsToAlwaysContentPolicy() {
        let item = FileProviderItem(entry: entry(materialized: true), parentKey: "docs/")
        XCTAssertEqual(item.contentPolicy, .downloadEagerlyAndKeepDownloaded)
        XCTAssertTrue(item.isDownloaded)
        XCTAssertEqual(item.itemIdentifier.rawValue, "docs/a.txt")
        XCTAssertEqual(item.filename, "a.txt")
        XCTAssertEqual(item.contentType, .data)
        XCTAssertEqual(item.documentSize, 5)
        XCTAssertEqual(item.parentItemIdentifier, NSFileProviderItemIdentifier("docs/"))
        XCTAssertTrue(item.isUploaded) // clean rows count as uploaded
        XCTAssertTrue(item.isDehydratable)
        XCTAssertEqual(item.creationDate, Date(timeIntervalSince1970: 1_728_211_200))
    }

    func testNonMaterializedFileMapsToDownloadsWhenOpened() {
        let item = FileProviderItem(entry: entry(materialized: false), parentKey: "docs/")
        XCTAssertEqual(item.contentPolicy, .downloadLazily)
        XCTAssertFalse(item.isDownloaded)
        XCTAssertFalse(item.isDehydratable) // nothing to dehydrate
    }

    func testDirtyFileNeverDehydratableAndReportsUploadingState() {
        let item = FileProviderItem(entry: entry(materialized: true, dirty: true), parentKey: "docs/")
        XCTAssertEqual(item.contentPolicy, .downloadEagerlyAndKeepDownloaded) // bytes are local
        XCTAssertFalse(item.isUploaded)
        XCTAssertFalse(item.isDehydratable) // the daemon refuses dirty evictions
    }

    func testDirEntryMapsToFolder() {
        let item = FileProviderItem(
            entry: entry(name: "sub", key: "docs/sub/", isDir: true, size: 0,
                         materialized: false, cachePath: ""),
            parentKey: "docs/")
        XCTAssertEqual(item.contentType, .folder)
        XCTAssertNil(item.documentSize)
        XCTAssertEqual(item.itemIdentifier.rawValue, "docs/sub/")
        XCTAssertTrue(item.parentItemIdentifier.rawValue == "docs/")
    }

    func testRootDirKeyIdentifierMapping() {
        XCTAssertEqual(FileProviderItem.identifier(forDirKey: ""), .rootContainer)
        XCTAssertEqual(
            FileProviderItem.dirKey(of: .rootContainer), "")
        XCTAssertEqual(
            FileProviderItem.dirKey(of: NSFileProviderItemIdentifier("docs")), "docs/")
        XCTAssertEqual(
            FileProviderItem.dirKey(of: NSFileProviderItemIdentifier("docs/")), "docs/")
    }

    // MARK: error mapping

    func testENOENTMapsToNoSuchItem() {
        let err = FileProviderErrorMapper.error(fromIPC: "no such item: docs/a.txt")
        XCTAssertEqual(err.domain, NSFileProviderErrorDomain)
        XCTAssertEqual(err.code, NSFileProviderError.noSuchItem.rawValue)
    }

    func testOtherErrorsSurfaceAsServerUnreachable() {
        let err = FileProviderErrorMapper.error(fromIPC: "download x: sync: refused")
        XCTAssertEqual(err.domain, NSFileProviderErrorDomain)
        XCTAssertEqual(err.code, NSFileProviderError.serverUnreachable.rawValue)
        XCTAssertEqual(err.localizedDescription, "download x: sync: refused")
    }
}

final class DownloadTriggerTests: XCTestCase {

    private var server: StubIPCServer!
    private var cacheDir: URL!
    private var mount: FileProviderMount!

    override func setUpWithError() throws {
        // macOS sockaddr_un.sun_path is ~104 bytes: keep the stub socket
        // path short (a /tmp dir, like the Go tests' shortSocket).
        let dir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("zfp-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        cacheDir = dir
        server = try StubIPCServer(path: dir.appendingPathComponent("z.ipc").path)
        mount = FileProviderMount(
            socketPath: server.path, cacheDir: dir)
    }

    override func tearDown() {
        server.stop()
        try? FileManager.default.removeItem(at: cacheDir)
    }

    func testDownloadTriggerHappyPathMaterializesFromCacheFile() throws {
        // The daemon hydrated the file into its cache dir; the extension
        // serves the bytes from there (data via the FILE).
        let cacheFile = cacheDir.appendingPathComponent("files/fp/remote.txt")
        try FileManager.default.createDirectory(
            at: cacheFile.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("remote body".utf8).write(to: cacheFile)

        server.respond(
            to: "download",
            with: "{\"v\":1,\"ok\":true,\"extra\":{\"path\":\"fp/remote.txt\",\"materialized\":true,\"cachePath\":\"files/fp/remote.txt\"}}")
        server.respond(
            to: "item",
            with: "{\"v\":1,\"ok\":true,\"extra\":{\"item\":{\"name\":\"remote.txt\",\"key\":\"fp/remote.txt\",\"isDir\":false,\"size\":11,\"mtime\":1728211200,\"materialized\":true,\"dirty\":false,\"cachePath\":\"files/fp/remote.txt\"}}}")

        let ack = try mount.download("fp/remote.txt")
        XCTAssertTrue(ack.materialized)
        XCTAssertEqual(ack.cachePath, "files/fp/remote.txt")

        let item = try mount.item("fp/remote.txt")
        let providerItem = FileProviderItem(entry: item, parentKey: "fp/")
        XCTAssertEqual(providerItem.contentPolicy, .downloadEagerlyAndKeepDownloaded)

        let url = mount.cacheURL(of: item)
        XCTAssertEqual(url, cacheFile)
        XCTAssertEqual(try Data(contentsOf: url), Data("remote body".utf8))
    }

    func testEnumerateOverStubServer() throws {
        server.respond(
            to: "enumerate",
            with: "{\"v\":1,\"ok\":true,\"extra\":{\"entries\":[" +
                "{\"name\":\"a.txt\",\"key\":\"docs/a.txt\",\"isDir\":false,\"size\":5,\"mtime\":1728211200,\"materialized\":true,\"dirty\":false,\"cachePath\":\"files/docs/a.txt\"}," +
                "{\"name\":\"sub\",\"key\":\"docs/sub/\",\"isDir\":true,\"size\":0,\"mtime\":0,\"materialized\":false,\"dirty\":false,\"cachePath\":\"\"}" +
                "]}}")
        let entries = try mount.enumerate(dirKey: "docs/")
        XCTAssertEqual(entries.count, 2)
        XCTAssertEqual(entries[0].key, "docs/a.txt")
        XCTAssertTrue(entries[1].isDir)
    }

    func testEnumerateFailureSurfacesFileProviderError() throws {
        server.respond(
            to: "enumerate",
            with: "{\"v\":1,\"ok\":false,\"error\":\"no such item: docs/missing\"}")
        XCTAssertThrowsError(try mount.enumerate(dirKey: "docs/missing")) { err in
            let ns = err as NSError
            XCTAssertEqual(ns.domain, NSFileProviderErrorDomain)
            XCTAssertEqual(ns.code, NSFileProviderError.noSuchItem.rawValue)
        }
    }

    func testMarkDeleteMoveAcksDecode() throws {
        server.respond(to: "mark", with: "{\"v\":1,\"ok\":true,\"extra\":{\"path\":\"fp/new.txt\",\"dirty\":true}}")
        server.respond(to: "delete", with: "{\"v\":1,\"ok\":true,\"extra\":{\"path\":\"fp/new.txt\"}}")
        server.respond(to: "move", with: "{\"v\":1,\"ok\":true,\"extra\":{\"from\":\"fp/old.txt\",\"to\":\"fp/new.txt\"}}")

        let mark = try mount.mark("fp/new.txt")
        XCTAssertEqual(mark.path, "fp/new.txt")
        XCTAssertTrue(mark.dirty)
        let del = try mount.delete("fp/new.txt")
        XCTAssertEqual(del.path, "fp/new.txt")
        let mv = try mount.move("fp/old.txt", to: "fp/new.txt")
        XCTAssertEqual(mv.from, "fp/old.txt")
        XCTAssertEqual(mv.to, "fp/new.txt")
    }

    func testDehydrateRefusalSurfacesVerbatim() throws {
        server.respond(
            to: "dehydrate",
            with: "{\"v\":1,\"ok\":false,\"error\":\"dehydrate x: refused, file is dirty (upload it first)\"}")
        XCTAssertThrowsError(try mount.dehydrate("fp/x.txt")) { err in
            XCTAssertEqual((err as NSError).userInfo[NSLocalizedDescriptionKey] as? String,
                           "dehydrate x: refused, file is dirty (upload it first)")
        }
    }
}
