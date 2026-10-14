// ReplicatedExtensionTests.swift - conformance tests for the MODERN
// Replicated extension (Sources/ZetaFileProviderReplicated), driven
// against the same in-process StubIPCServer the leaf-02 tests use. The
// Replicated protocol's completion-handler surface is exercised
// end-to-end: item lookup, content fetch, create/modify/delete, and
// the copy-through-cache write path.

import FileProvider
import UniformTypeIdentifiers
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
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"doc.txt","key":"doc.txt","isDir":false,"size":9,"mtime":1728211200,"materialized":true,"dirty":true,"cachePath":"files/doc.txt"}}}"##)

        let template = ReplicatedTemplate(filename: "doc.txt", parent: .rootContainer)
        let exp = expectation(description: "create")
        _ = ext.createItem(basedOn: template, fields: [.contents], contents: localFile, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(item?.filename, "doc.txt")
            // The bytes must have landed in the daemon's DATA plane before
            // mark: <cacheDir>/files/<key> - the exact path the daemon's
            // mark handler stats (H6; the fixtures used to pin the wrong
            // cacheDir/<key> path).
            let cached = try? Data(contentsOf: self.cacheDir.appendingPathComponent("files/doc.txt"))
            XCTAssertEqual(cached, Data("new bytes".utf8))
            // And nothing may strand at the OLD (pre-H6) location.
            XCTAssertFalse(FileManager.default.fileExists(
                atPath: self.cacheDir.appendingPathComponent("doc.txt").path))
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    /// F1: a nested create's parent identifier is a DIR identifier, which
    /// carries a TRAILING slash (FileProviderItem.identifier(forDirKey:)
    /// emits "fp/"). Appending "/<name>" to it verbatim produced the
    /// double-slash key "fp//doc.txt" - stored literally by the daemon and
    /// never surfaced by Enumerate. The parent must be normalized so the
    /// key (and the mark, and the cache path) is "fp/doc.txt".
    func testCreateItemNestedKeyWritesUnderFilesSegmentAndMarks() throws {
        let localFile = cacheDir.appendingPathComponent("upload-src.txt")
        try Data("nested bytes".utf8).write(to: localFile)
        server.respond(to: "mark", with: #"{"v":1,"ok":true,"extra":{"path":"fp/doc.txt","dirty":true}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"doc.txt","key":"fp/doc.txt","isDir":false,"size":12,"mtime":1728211200,"materialized":true,"dirty":true,"cachePath":"files/fp/doc.txt"}}}"##)

        // The REAL parent-identifier shape production emits: a dir
        // identifier with the trailing slash (NOT a bare "fp").
        let template = ReplicatedTemplate(
            filename: "doc.txt",
            parent: FileProviderItem.identifier(forDirKey: "fp"))
        XCTAssertEqual(template.parentItemIdentifier.rawValue, "fp/")
        let exp = expectation(description: "create-nested")
        _ = ext.createItem(basedOn: template, fields: [.contents], contents: localFile, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            // Key pins: no double slash anywhere.
            XCTAssertEqual(item?.itemIdentifier.rawValue, "fp/doc.txt")
            XCTAssertEqual(item?.parentItemIdentifier.rawValue, "fp/")
            let cached = try? Data(contentsOf: self.cacheDir.appendingPathComponent("files/fp/doc.txt"))
            XCTAssertEqual(cached, Data("nested bytes".utf8))
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        // The mark key must be the clean single-slash form too.
        let marks = server.requests.filter { $0.type == "mark" }
        XCTAssertEqual(marks.map { $0.path }, ["fp/doc.txt"])
    }

    /// F2: creating a directory (createItem with no contents) must NOT
    /// call mark. The daemon's wire layer normalizes the mark path
    /// (normalizePath strips a trailing slash before Mark sees it) and its
    /// Mark handler stats a plain cache file and writes a Dirty FILE-shaped
    /// row - so a marked dir makes the prompt-upload loop PUT a directory
    /// forever. Dirs are derived from the remote namespace by the daemon's
    /// enumerate; the local mirror dir is just cache. The returned item is
    /// the daemon's dir-entry shape: trailing-slash key, clean,
    /// unmaterialized, empty cachePath.
    func testCreateItemWithoutContentsCreatesDirectory() throws {
        let template = ReplicatedTemplate(filename: "docs", parent: .rootContainer)
        let exp = expectation(description: "mkdir")
        _ = ext.createItem(basedOn: template, fields: [], contents: nil, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            // The returned item uses the daemon's trailing-slash dir-key
            // convention (the enumerate.json golden's dir-entry shape).
            XCTAssertEqual(item?.itemIdentifier.rawValue, "docs/")
            XCTAssertEqual(item?.contentType, UTType.folder)
            XCTAssertEqual(item?.isUploaded, true) // never dirty
            var isDir: ObjCBool = false
            // H6: the dir key maps under files/ too (the daemon's data
            // plane is flat about this: EVERY key lives under files/).
            XCTAssertTrue(FileManager.default.fileExists(atPath: self.cacheDir.appendingPathComponent("files/docs").path, isDirectory: &isDir))
            XCTAssertTrue(isDir.boolValue)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        // The pinned convention: NO mark IPC at all for a directory.
        XCTAssertTrue(server.requests.filter { $0.type == "mark" }.isEmpty)
    }

    /// Fix 4 (collision): creating an item whose destination ALREADY exists
    /// in the data plane is a name collision - the extension must fail with
    /// NSFileProviderError(.filenameCollision) and issue NO mark/item IPC,
    /// instead of silently removing+overwriting the existing object.
    func testCreateItemOverExistingDestinationFailsWithCollision() throws {
        // Pre-state: the destination already has bytes on the data plane.
        try FileManager.default.createDirectory(
            at: cacheDir.appendingPathComponent("files"), withIntermediateDirectories: true)
        try Data("existing bytes".utf8).write(to: cacheDir.appendingPathComponent("files/doc.txt"))

        let localFile = cacheDir.appendingPathComponent("upload-src.txt")
        try Data("new bytes".utf8).write(to: localFile)

        let template = ReplicatedTemplate(filename: "doc.txt", parent: .rootContainer)
        let exp = expectation(description: "collision")
        _ = ext.createItem(basedOn: template, fields: [.contents], contents: localFile, options: [], request: NSFileProviderRequest()) { item, _, _, error in
            let ns = error as NSError?
            XCTAssertNotNil(ns)
            XCTAssertEqual(ns?.domain, NSFileProviderErrorDomain)
            XCTAssertEqual(ns?.code, NSFileProviderError.filenameCollision.rawValue)
            XCTAssertNil(item)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        // The existing object's bytes must be untouched...
        let untouched = try Data(contentsOf: cacheDir.appendingPathComponent("files/doc.txt"))
        XCTAssertEqual(untouched, Data("existing bytes".utf8))
        // ...and no mark/item may have been issued (the collision short-
        // circuits before any IPC).
        XCTAssertTrue(server.requests.filter { $0.type == "mark" }.isEmpty)
        XCTAssertTrue(server.requests.filter { $0.type == "item" }.isEmpty)
    }

    /// Fix 4 (collision), the mayAlreadyExist branch: the SAME situation
    /// with the system's .mayAlreadyExist vouch must proceed through the
    /// normal write path (copy + mark + item).
    func testCreateItemWithMayAlreadyExistOverwrites() throws {
        try FileManager.default.createDirectory(
            at: cacheDir.appendingPathComponent("files"), withIntermediateDirectories: true)
        try Data("existing bytes".utf8).write(to: cacheDir.appendingPathComponent("files/doc.txt"))

        let localFile = cacheDir.appendingPathComponent("upload-src.txt")
        try Data("new bytes".utf8).write(to: localFile)
        server.respond(to: "mark", with: #"{"v":1,"ok":true,"extra":{"path":"doc.txt","dirty":true}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"doc.txt","key":"doc.txt","isDir":false,"size":9,"mtime":1728211200,"materialized":true,"dirty":true,"cachePath":"files/doc.txt"}}}"##)

        let template = ReplicatedTemplate(filename: "doc.txt", parent: .rootContainer)
        let exp = expectation(description: "may-already-exist")
        _ = ext.createItem(basedOn: template, fields: [.contents], contents: localFile, options: [.mayAlreadyExist], request: NSFileProviderRequest()) { item, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(item?.filename, "doc.txt")
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        let overwritten = try Data(contentsOf: cacheDir.appendingPathComponent("files/doc.txt"))
        XCTAssertEqual(overwritten, Data("new bytes".utf8))
        XCTAssertEqual(server.requests.filter { $0.type == "mark" }.map { $0.path }, ["doc.txt"])
    }

    // MARK: - modifyItem

    func testModifyItemFilenameMoveRenamesThroughIPC() throws {
        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"doc.txt","to":"renamed.txt"}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"renamed.txt","key":"renamed.txt","isDir":false,"size":11,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"files/renamed.txt"}}}"##)

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

    /// H7: rename + content in ONE modifyItem - the IPC move re-points
    /// the index first, so the content copy and mark must target the NEW
    /// key (the old key is a tombstone after the move). The stub cannot
    /// inspect request bodies, so the proof is on the data plane: the
    /// bytes land under files/<newKey>, and the final item lookup (which
    /// the extension issues for the key it marked) only succeeds for the
    /// NEW key.
    func testModifyItemRenameThenContentMarksTheNewKey() throws {
        // Pre-state: the item's bytes live under the OLD key.
        try FileManager.default.createDirectory(
            at: cacheDir.appendingPathComponent("files"), withIntermediateDirectories: true)
        try Data("old body".utf8).write(to: cacheDir.appendingPathComponent("files/doc.txt"))

        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"doc.txt","to":"renamed.txt"}}"#)
        server.respond(to: "mark", with: #"{"v":1,"ok":true,"extra":{"path":"renamed.txt","dirty":true}}"#)
        // The extension's final item(key) call: only the NEW key resolves.
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"renamed.txt","key":"renamed.txt","isDir":false,"size":9,"mtime":1728211200,"materialized":true,"dirty":true,"cachePath":"files/renamed.txt"}}}"##)

        let item = FileProviderItem(entry: entry(name: "renamed.txt", key: "doc.txt"), parentKey: "")
        let newContents = cacheDir.appendingPathComponent("rename-src.txt")
        try Data("new body".utf8).write(to: newContents)

        let exp = expectation(description: "rename+content")
        _ = ext.modifyItem(
            item,
            baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)),
            changedFields: [.filename, .contents],
            contents: newContents, options: [], request: NSFileProviderRequest()) { modified, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(modified?.filename, "renamed.txt")
            // The content write must have re-pointed to the NEW key...
            let renamed = try? Data(contentsOf: self.cacheDir.appendingPathComponent("files/renamed.txt"))
            XCTAssertEqual(renamed, Data("new body".utf8))
            // ...and must NOT have resurrected the tombstoned old key
            // with the new bytes.
            let old = try? Data(contentsOf: self.cacheDir.appendingPathComponent("files/doc.txt"))
            XCTAssertEqual(old, Data("old body".utf8))
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    /// F1, rename half: the same trailing-slash dir-identifier shape as
    /// the nested-create pin, on modifyItem - a rename INSIDE a nested
    /// folder must produce "fp/renamed.txt" (single slash), not
    /// "fp//renamed.txt".
    func testModifyItemNestedRenameBuildsSingleSlashKey() throws {
        // Pre-state: the item's bytes live under the old nested key.
        try FileManager.default.createDirectory(
            at: cacheDir.appendingPathComponent("files/fp"), withIntermediateDirectories: true)
        try Data("nested body".utf8).write(to: cacheDir.appendingPathComponent("files/fp/doc.txt"))

        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"fp/doc.txt","to":"fp/renamed.txt"}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"renamed.txt","key":"fp/renamed.txt","isDir":false,"size":12,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"files/fp/renamed.txt"}}}"##)

        let item = FileProviderItem(
            entry: entry(name: "renamed.txt", key: "fp/doc.txt"),
            parentKey: "fp/")
        XCTAssertEqual(item.parentItemIdentifier.rawValue, "fp/") // the dir-identifier shape
        let exp = expectation(description: "nested-rename")
        _ = ext.modifyItem(
            item,
            baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)),
            changedFields: [.filename],
            contents: nil, options: [], request: NSFileProviderRequest()) { modified, _, _, error in
            XCTAssertNil(error)
            XCTAssertEqual(modified?.itemIdentifier.rawValue, "fp/renamed.txt")
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        // F1 pins (the stub does not emulate the daemon's cache-file
        // rename, so the contract lives in the IPC keys): the move must
        // carry the clean single-slash pair, and the post-move item
        // lookup must target the NEW key.
        let moves = server.requests.filter { $0.type == "move" }
        XCTAssertEqual(moves.count, 1)
        XCTAssertEqual(moves.first?.path, "fp/doc.txt")
        XCTAssertEqual(moves.first?.to, "fp/renamed.txt")
        let lookups = server.requests.filter { $0.type == "item" }
        XCTAssertEqual(lookups.map { $0.path }, ["fp/renamed.txt"])
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

    /// W2-M11, move-only half: a .parentItemIdentifier-only change (the
    /// Finder drag shape) must issue ONE move whose destination is the NEW
    /// parent + the UNCHANGED filename, and the returned item must be the
    /// item at the FINAL key (the old key is a tombstone after the move -
    /// an item lookup on it would fail against the real daemon).
    func testModifyItemParentOnlyChangeMovesToNewFolder() throws {
        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"docs/a.txt","to":"archive/a.txt"}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"a.txt","key":"archive/a.txt","isDir":false,"size":11,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"files/archive/a.txt"}}}"##)

        // The item's identity is the OLD key (publicKey is what the
        // extension moves FROM); the modify's item already carries the NEW
        // parent (the dir-identifier shape production emits: "archive/")
        // - NSFileProviderReplicatedExtension modify semantics, same as
        // the existing rename pins.
        let item = FileProviderItem(entry: entry(name: "a.txt", key: "docs/a.txt"), parentKey: "archive/")
        let exp = expectation(description: "move-only")
        _ = ext.modifyItem(
            item,
            baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)),
            changedFields: [.parentItemIdentifier],
            contents: nil, options: [], request: NSFileProviderRequest()) { modified, _, _, error in
                XCTAssertNil(error)
                XCTAssertEqual(modified?.itemIdentifier.rawValue, "archive/a.txt")
                XCTAssertEqual(modified?.filename, "a.txt")
                exp.fulfill()
            }
        wait(for: [exp], timeout: 5)

        // ONE move, old key -> new-parent/filename. No rename happened, so
        // the leaf component must be identical on both sides.
        let moves = server.requests.filter { $0.type == "move" }
        XCTAssertEqual(moves.count, 1)
        XCTAssertEqual(moves.first?.path, "docs/a.txt")
        XCTAssertEqual(moves.first?.to, "archive/a.txt")
        // The returned item is looked up at the FINAL key only.
        XCTAssertEqual(server.requests.filter { $0.type == "item" }.map { $0.path }, ["archive/a.txt"])
    }

    /// W2-M11, rename+move half: a modify that changes BOTH the filename
    /// and the parent must land at the fully-new key with exactly ONE move
    /// - two chained moves would strand a second tombstone at the
    /// intermediate key for the sync to push as a spurious remote DELETE.
    func testModifyItemRenameAndMoveIsOneMoveToTheFinalKey() throws {
        server.respond(to: "move", with: #"{"v":1,"ok":true,"extra":{"from":"docs/old.txt","to":"archive/new.txt"}}"#)
        server.respond(to: "item", with:
            ##"{"v":1,"ok":true,"extra":{"item":{"name":"new.txt","key":"archive/new.txt","isDir":false,"size":11,"mtime":1728211200,"materialized":true,"dirty":false,"cachePath":"files/archive/new.txt"}}}"##)

        let item = FileProviderItem(entry: entry(name: "new.txt", key: "docs/old.txt"), parentKey: "archive/")

        let exp = expectation(description: "rename+move")
        _ = ext.modifyItem(
            item,
            baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)),
            changedFields: [.filename, .parentItemIdentifier],
            contents: nil, options: [], request: NSFileProviderRequest()) { modified, _, _, error in
                XCTAssertNil(error)
                XCTAssertEqual(modified?.itemIdentifier.rawValue, "archive/new.txt")
                exp.fulfill()
            }
        wait(for: [exp], timeout: 5)

        let moves = server.requests.filter { $0.type == "move" }
        XCTAssertEqual(moves.count, 1, "rename+move in one modify must be ONE IPC move")
        XCTAssertEqual(moves.first?.path, "docs/old.txt")
        XCTAssertEqual(moves.first?.to, "archive/new.txt")
        XCTAssertEqual(server.requests.filter { $0.type == "item" }.map { $0.path }, ["archive/new.txt"])
    }

    // MARK: - deleteItem (W2-M12: folders)

    /// W2-M12: deleting a FOLDER must NOT go through the file-shaped IPC
    /// delete. The daemon has NO dir-delete capability (its only "delete"
    /// op normalizes away the trailing slash, os.Remove's the cache path -
    /// ENOTEMPTY on a non-empty mirror dir - and tombstones a FILE-shaped
    /// key that strands the children's rows), so the extension refuses the
    /// folder delete with .cannotSynchronize. Pinned: the trailing-slash
    /// identifier produces an NSFileProviderError-domain error and ZERO IPC
    /// traffic - no delete, no enumerate.
    func testDeleteItemFolderIsRefusedWithoutIPC() throws {
        let exp = expectation(description: "delete-folder-refused")
        _ = ext.deleteItem(identifier: NSFileProviderItemIdentifier("docs/"), baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)), options: [], request: NSFileProviderRequest()) { error in
            let ns = error as NSError?
            XCTAssertNotNil(ns)
            XCTAssertEqual(ns?.domain, NSFileProviderErrorDomain)
            XCTAssertEqual(ns?.code, NSFileProviderError.cannotSynchronize.rawValue)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        XCTAssertTrue(server.requests.filter { $0.type == "delete" }.isEmpty)
        XCTAssertTrue(server.requests.filter { $0.type == "enumerate" }.isEmpty)
    }

    /// W2-M12: a FILE delete must stay one-shot - no enumeration for a
    /// file-shaped key.
    func testDeleteItemFileKeyDoesNotEnumerate() throws {
        server.respond(to: "delete", with: #"{"v":1,"ok":true,"extra":{"path":"doc.txt"}}"#)

        let exp = expectation(description: "delete-file")
        _ = ext.deleteItem(identifier: NSFileProviderItemIdentifier("doc.txt"), baseVersion: NSFileProviderItemVersion(contentVersion: Data("v1".utf8), metadataVersion: Data("v1".utf8)), options: [], request: NSFileProviderRequest()) { error in
            XCTAssertNil(error)
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
        XCTAssertTrue(server.requests.filter { $0.type == "enumerate" }.isEmpty)
        XCTAssertEqual(server.requests.filter { $0.type == "delete" }.map { $0.path }, ["doc.txt"])
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
    // cachePath is the daemon's data-plane path: "files/<key>" (H6).
    let json = "{\"name\":\"" + name + "\",\"key\":\"" + key
        + "\",\"isDir\":false,\"size\":11,\"mtime\":1728211200,\"materialized\":true,\"dirty\":false,\"cachePath\":\"files/" + key + "\"}"
    return try! JSONDecoder().decode(FileProviderItemEntry.self, from: Data(json.utf8))
}

// MARK: - cacheURL mapping (H6) + sync anchor (L43)

final class CachePathMappingTests: XCTestCase {
    func testCacheURLOfKeyIncludesFilesSegment() {
        let cacheDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("zfpH6-\(UUID().uuidString.prefix(8))")
        let mount = FileProviderMount(socketPath: "/unused", cacheDir: cacheDir)
        // Top-level key...
        XCTAssertEqual(
            mount.cacheURL(ofKey: "doc.txt"),
            cacheDir.appendingPathComponent("files/doc.txt"))
        // ...and nested key: "files/" is PREPENDED, not swallowed.
        XCTAssertEqual(
            mount.cacheURL(ofKey: "fp/doc.txt"),
            cacheDir.appendingPathComponent("files/fp/doc.txt"))
        // And it matches the wire's cachePath shape for the same key.
        let entry = try! JSONDecoder().decode(
            FileProviderItemEntry.self,
            from: Data(##"{"name":"doc.txt","key":"fp/doc.txt","isDir":false,"size":1,"mtime":1,"materialized":true,"dirty":false,"cachePath":"files/fp/doc.txt"}"##.utf8))
        XCTAssertEqual(
            mount.cacheURL(of: entry), mount.cacheURL(ofKey: entry.key))
    }

    func testCacheURLCreatingParentsCreatesIntermediateDirs() throws {
        let cacheDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("zfpH6-\(UUID().uuidString.prefix(8))")
        let mount = FileProviderMount(socketPath: "/unused", cacheDir: cacheDir)
        let url = try mount.cacheURLCreatingParents(ofKey: "a/b/c.txt")
        XCTAssertTrue(FileManager.default.fileExists(atPath: url.deletingLastPathComponent().path))
    }
}

final class EnumeratorAnchorTests: XCTestCase {
    private func entry(_ key: String, mtime: Int64, size: Int64 = 5) -> FileProviderItemEntry {
        var json = "{\"name\":\"n\",\"key\":\"" + key
        json += "\",\"isDir\":false,\"size\":" + String(size)
        json += ",\"mtime\":" + String(mtime)
        json += ",\"materialized\":true,\"dirty\":false,\"cachePath\":\"files/" + key + "\"}"
        return try! JSONDecoder().decode(FileProviderItemEntry.self, from: Data(json.utf8))
    }

    func testAnchorIsDeterministicAndContentSensitive() {
        let a = [entry("b.txt", mtime: 1), entry("a.txt", mtime: 2)]
        // Order-independent (sorted rows)...
        XCTAssertEqual(
            FileProviderEnumerator.anchor(for: a),
            FileProviderEnumerator.anchor(for: a.reversed()))
        // ...but sensitive to set, mtime, and size.
        let withExtra = a + [entry("c.txt", mtime: 3)]
        XCTAssertNotEqual(
            FileProviderEnumerator.anchor(for: a),
            FileProviderEnumerator.anchor(for: withExtra))
        let touched = [entry("b.txt", mtime: 9), entry("a.txt", mtime: 2)]
        XCTAssertNotEqual(
            FileProviderEnumerator.anchor(for: a),
            FileProviderEnumerator.anchor(for: touched))
    }


    /// W2-H3: a DIR key's trailing slash is its OWN - parentKeyOf must strip
    /// it before cutting, or the dir becomes its own child.
    func testParentKeyOfDirKeyIsNotSelf() {
        XCTAssertEqual(parentKeyOf("docs/"), "")
        XCTAssertEqual(parentKeyOf("a/b/"), "a")
        XCTAssertEqual(parentKeyOf("a/b.txt"), "a")
        XCTAssertEqual(parentKeyOf("top.txt"), "")
    }
}
