// FileProviderExtension.swift - the NSFileProviderExtension-shaped
// subclass bridging Finder to the zeta-cache daemon (fileprovider-2026-10
// leaf 02). THE THIN-ADAPTER RULES (locked):
//   - No network, no credentials: every byte of data flows via the
//     daemon's cache files (same user); every decision via IPC.
//   - Data via the FILE, metadata via IPC: an entry's cachePath is
//     relative to the daemon's cacheDir; this extension opens
//     <cacheDir>/<cachePath> directly.
//
// SUBCLASS SHIM: the class extends ZetaFPShim (the availability shim
// target) - the macOS 26 SDK marks NSFileProviderExtension unavailable
// to Swift; the shim redeclares the identical selector surface. At
// runtime, overrides dispatch by selector, so a registered extension
// (leaf 03) behaves as a real NSFileProviderExtension subclass.
//
// importDoesUserVisibleSemanticRename semantics (documented): this
// extension does NOT implement user-visible semantic rename imports -
// a Finder rename maps to IPC move (below), which re-points the daemon's
// index row in one step; there is no second import pass, so the flag
// stays at its default (false) and no special rename handling is needed.

import FileProvider
import Foundation
import ZetaFPShim

/// One live connection to the daemon: socket path + cache dir. Injected
/// (never discovered here) so tests drive a stub server.
public struct FileProviderMount {
    public let socketPath: String
    public let cacheDir: URL

    public init(socketPath: String, cacheDir: URL) {
        self.socketPath = socketPath
        self.cacheDir = cacheDir
    }

    /// Default mount: the macOS default IPC socket (/tmp/zeta-cache.ipc)
    /// and the daemon's default cache dir.
    public static func standard() -> FileProviderMount {
        let home = FileManager.default.homeDirectoryForCurrentUser
        return FileProviderMount(
            socketPath: ProcessInfo.processInfo.environment["ZETA_CACHE_IPC"]
                ?? "/tmp/zeta-cache.ipc",
            cacheDir: home.appendingPathComponent("Library/Application Support/zeta-cache"))
    }

    // MARK: IPC wrappers (each one request per connection)

    func enumerate(dirKey: String) throws -> [FileProviderItemEntry] {
        let resp = try IPCClient.ask(socketPath, .enumerate(dirKey))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "enumerate failed") }
        guard case .enumerate(let data) = resp.extra else {
            throw IPCClient.Failure("enumerate: unexpected payload")
        }
        return data.entries
    }

    func item(_ path: String) throws -> FileProviderItemEntry {
        let resp = try IPCClient.ask(socketPath, .item(path))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "item failed") }
        guard case .item(let data) = resp.extra else {
            throw IPCClient.Failure("item: unexpected payload")
        }
        return data.item
    }

    /// Blocking hydration: the daemon answers only when the cache file is
    /// materialized (or on its ctx timeout - surfaced as ok:false).
    @discardableResult
    func download(_ path: String) throws -> DownloadAck {
        let resp = try IPCClient.ask(socketPath, .download(path))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "download failed") }
        guard case .download(let ack) = resp.extra else {
            throw IPCClient.Failure("download: unexpected payload")
        }
        return ack
    }

    func dehydrate(_ path: String) throws {
        let resp = try IPCClient.ask(socketPath, .dehydrate(path))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "dehydrate failed") }
    }

    @discardableResult
    func mark(_ path: String) throws -> MarkAck {
        let resp = try IPCClient.ask(socketPath, .mark(path))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "mark failed") }
        guard case .mark(let ack) = resp.extra else {
            throw IPCClient.Failure("mark: unexpected payload")
        }
        return ack
    }

    @discardableResult
    func delete(_ path: String) throws -> DeleteAck {
        let resp = try IPCClient.ask(socketPath, .delete(path))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "delete failed") }
        guard case .delete(let ack) = resp.extra else {
            throw IPCClient.Failure("delete: unexpected payload")
        }
        return ack
    }

    @discardableResult
    func move(_ path: String, to dest: String) throws -> MoveAck {
        let resp = try IPCClient.ask(socketPath, .move(path, to: dest))
        guard resp.ok else { throw FileProviderErrorMapper.error(fromIPC: resp.error ?? "move failed") }
        guard case .move(let ack) = resp.extra else {
            throw IPCClient.Failure("move: unexpected payload")
        }
        return ack
    }

    // MARK: cache-file access (the data plane)

    /// The absolute URL of an entry's backing file.
    func cacheURL(of entry: FileProviderItemEntry) -> URL {
        cacheDir.appendingPathComponent(entry.cachePath)
    }
}

open class FileProviderExtension: ZetaFPShim {
    /// The daemon mount. Overridable for tests (a stub IPC server).
    open var mount: FileProviderMount = .standard()

    /// The extension's domain (comes from the system once registered).
    open override var domain: NSFileProviderDomain? { nil }

    /// The container being enumerated for this extension instance (set
    /// by the framework when it instantiates per-container extensions).
    open var enumeratedItemIdentifier: NSFileProviderItemIdentifier = .rootContainer

    // MARK: enumeration

    /// Enumerate a container via IPC enumerate (the daemon's cached
    /// index; materialized and pending items share one view - a
    /// non-materialized entry carries contentPolicy .downloadsWhenOpened,
    /// so the framework's materialized/pending split collapses to one
    /// enumeration).
    open override func enumerator(forContainerItemIdentifier containerItemIdentifier: NSFileProviderItemIdentifier) throws -> NSFileProviderEnumerator {
        FileProviderEnumerator(containerItemIdentifier: containerItemIdentifier, mount: mount)
    }

    // MARK: identity <-> URL

    open override func urlForItem(withPersistentIdentifier identifier: NSFileProviderItemIdentifier) -> URL? {
        guard let entry = try? mount.item(identifier.rawValue) else { return nil }
        return mount.cacheURL(of: entry)
    }

    open override func persistentIdentifierForItem(at url: URL) -> NSFileProviderItemIdentifier? {
        let filesBase = mount.cacheDir.path + "/files/"
        guard url.path.hasPrefix(filesBase) else { return nil }
        return NSFileProviderItemIdentifier(String(url.path.dropFirst(filesBase.count)))
    }

    // MARK: Lookup

    /// The Lookup analog: IPC item -> FileProviderItem.
    open override func item(forIdentifier identifier: NSFileProviderItemIdentifier) throws -> NSFileProviderItem {
        let entry = try mount.item(identifier.rawValue)
        return FileProviderItem(entry: entry, parentKey: FileProviderItem.parentDirKey(of: entry.key))
    }

    // MARK: - Download trigger (contentPolicy .downloadsWhenOpened)

    /// The system calls this when the bytes are first needed: block on
    /// IPC download (the daemon hydrates through its engine), then the
    /// file is served from the cache dir.
    open override func startProvidingItem(at url: URL, completionHandler: @escaping ((Error?) -> Void)) {
        guard let key = persistentIdentifierForItem(at: url)?.rawValue else {
            completionHandler(FileProviderErrorMapper.error(fromIPC: "no such item: \(url.lastPathComponent)"))
            return
        }
        do {
            _ = try mount.download(key)
            completionHandler(nil)
        } catch {
            completionHandler(error)
        }
    }

    /// No-op: the daemon owns eviction policy (leaf 07); the extension
    /// only reports explicit eviction (below).
    open override func stopProvidingItem(at url: URL) {}

    // MARK: - Eviction ("Remove Download" in Finder)

    /// The system's evict request -> IPC dehydrate (the daemon refuses
    /// dirty/pinned/tombstoned paths; that error surfaces verbatim).
    open override func evictItem(withIdentifier identifier: NSFileProviderItemIdentifier, completionHandler: @escaping ((Error?) -> Void)) {
        do {
            try mount.dehydrate(identifier.rawValue)
            completionHandler(nil)
        } catch {
            completionHandler(error)
        }
    }

    // MARK: - Mutations (create / delete / rename)

    /// CREATE (with content): write the bytes into the daemon's cache
    /// dir directly (same user - data via the FILE), then IPC mark so
    /// the daemon's prompt-upload loop pushes them.
    open override func createItemBased(onTemplate itemTemplate: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?, options: NSFileProviderCreateItemOptions = [], completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, Error?) -> Void) {
        let dirKey = FileProviderItem.dirKey(of: itemTemplate.parentItemIdentifier)
        let key = dirKey + itemTemplate.filename
        do {
            let dst = mount.cacheDir.appendingPathComponent("files").appendingPathComponent(key)
            try FileManager.default.createDirectory(
                at: dst.deletingLastPathComponent(), withIntermediateDirectories: true)
            if let src = url {
                if FileManager.default.fileExists(atPath: dst.path) {
                    try FileManager.default.removeItem(at: dst)
                }
                try FileManager.default.copyItem(at: src, to: dst)
            } else {
                try Data().write(to: dst)
            }
            _ = try mount.mark(key)
            let entry = try mount.item(key)
            completionHandler(FileProviderItem(entry: entry, parentKey: dirKey), [], false, nil)
        } catch {
            completionHandler(nil, [], false, error)
        }
    }

    /// DELETE: cache-file removal + tombstone via IPC delete (the sync's
    /// matrix-6 pass issues the remote DELETE).
    open override func deleteItem(withIdentifier identifier: NSFileProviderItemIdentifier, completionHandler: @escaping ((Error?) -> Void)) {
        do {
            _ = try mount.delete(identifier.rawValue)
            completionHandler(nil)
        } catch {
            completionHandler(error)
        }
    }

    /// RENAME: cache rename + index re-point via IPC move (the sync then
    /// deletes the old key remotely and uploads the new one).
    open override func renameItem(withIdentifier itemIdentifier: NSFileProviderItemIdentifier, toName itemName: String, completionHandler: @escaping (NSFileProviderItem?, Error?) -> Void) {
        let key = itemIdentifier.rawValue
        let dirKey = FileProviderItem.parentDirKey(of: key)
        let dest = dirKey + itemName
        do {
            _ = try mount.move(key, to: dest)
            let entry = try mount.item(dest)
            completionHandler(FileProviderItem(entry: entry, parentKey: dirKey), nil)
        } catch {
            completionHandler(nil, error)
        }
    }
}
