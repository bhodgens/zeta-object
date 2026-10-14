// FileProviderReplicatedExtension.swift - the MODERN Replicated extension
// (NSFileProviderReplicatedExtension, macOS 11+): the follow-up to the
// classic-API extension (leaf 02, FileProviderMount). Same thin-adapter
// contract as the rest of zeta-cache: the daemon owns network +
// credentials; this process speaks only the local IPC protocol (via the
// shared FileProviderMount) and reads/writes the daemon's cache dir.
// Issue #14 remainder.
//
// Signatures verified against this machine's SDK (macOS 26.2): every
// protocol method carries an NSFileProviderRequest and reports through a
// completion handler; the protocol's Swift surface takes identifiers,
// not items, on fetch.

import FileProvider
import Foundation
import ZetaFileProvider

@objc(FPReplicatedExtension)
public final class ReplicatedExtension: NSObject, NSFileProviderReplicatedExtension {
    let domain: NSFileProviderDomain
    let mount: FileProviderMount

    public required init(domain: NSFileProviderDomain) {
        self.domain = domain
        self.mount = .standard()
        super.init()
    }

    public init(domain: NSFileProviderDomain, mount: FileProviderMount) {
        // Test seam: inject a stub-backed mount.
        self.domain = domain
        self.mount = mount
        super.init()
    }

    public func invalidate() {
        // No persistent state: IPC connections are per-request.
    }

    // MARK: - Enumeration (NSFileProviderEnumerating)

    public func enumerator(for containerItemIdentifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest) throws -> NSFileProviderEnumerator {
        FileProviderEnumerator(containerItemIdentifier: containerItemIdentifier, mount: mount)
    }

    // MARK: - Items

    public func item(for identifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                let entry = try mount.item(identifier.rawValue)
                progress.completedUnitCount = 100
                completionHandler(FileProviderItem(entry: entry, parentKey: parentKeyOf(entry.key)), nil)
            } catch {
                progress.completedUnitCount = 100
                completionHandler(nil, error)
            }
        }
        return progress
    }

    // MARK: - Fetch (materialization)

    public func fetchContents(for itemIdentifier: NSFileProviderItemIdentifier, version requestedVersion: NSFileProviderItemVersion?, request: NSFileProviderRequest, completionHandler: @escaping (URL?, NSFileProviderItem?, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                let ack = try mount.download(itemIdentifier.rawValue)
                let entry = try mount.item(itemIdentifier.rawValue)
                progress.completedUnitCount = 100
                completionHandler(
                    mount.cacheDir.appendingPathComponent(ack.cachePath),
                    FileProviderItem(entry: entry, parentKey: parentKeyOf(entry.key)),
                    nil)
            } catch {
                progress.completedUnitCount = 100
                completionHandler(nil, nil, error)
            }
        }
        return progress
    }

    // MARK: - Mutations

    public func createItem(basedOn template: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?, options: NSFileProviderCreateItemOptions, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                let parentKey = parentKeyForChild(template.parentItemIdentifier.rawValue)
                let key = parentKey.isEmpty ? template.filename : parentKey + "/" + template.filename
                if let url {
                    // H6: bytes land at <cacheDir>/files/<key> (the daemon's
                    // data plane), parents created on demand.
                    let dst = try mount.cacheURLCreatingParents(ofKey: key)
                    if FileManager.default.fileExists(atPath: dst.path) {
                        try FileManager.default.removeItem(at: dst)
                    }
                    try FileManager.default.copyItem(at: url, to: dst)
                    try mount.mark(key)
                    let entry = try mount.item(key)
                    progress.completedUnitCount = 100
                    // The index is authoritative for every field: nothing left
                    // for the system to fetch back (inducted=false).
                    completionHandler(FileProviderItem(entry: entry, parentKey: parentKeyOf(entry.key)), [], false, nil)
                } else {
                    // F2: directories are NEVER marked. The daemon's wire
                    // layer normalizes the mark path (normalizePath strips a
                    // trailing slash before Mark sees it) and its Mark
                    // handler stats a plain cache file and writes a Dirty
                    // FILE-shaped row - a marked dir makes the prompt-upload
                    // loop PUT a directory forever. The daemon's enumerate
                    // derives dirs from the remote namespace; this local
                    // mirror dir is just cache. A fresh dir has no index row
                    // yet, so the item is reported locally (trailing-slash
                    // key, the daemon's dir convention; clean, unmaterialized,
                    // empty cachePath - the enumerate dir-entry shape).
                    try FileManager.default.createDirectory(
                        at: mount.cacheURL(ofKey: key), withIntermediateDirectories: true)
                    let entry = FileProviderItemEntry(
                        name: template.filename, key: key + "/", isDir: true,
                        size: 0, mtime: Int64(Date().timeIntervalSince1970),
                        materialized: false, dirty: false, cachePath: "")
                    progress.completedUnitCount = 100
                    completionHandler(FileProviderItem(entry: entry, parentKey: parentKey), [], false, nil)
                }
            } catch {
                progress.completedUnitCount = 100
                completionHandler(nil, [], false, error)
            }
        }
        return progress
    }

    public func modifyItem(_ item: NSFileProviderItem, baseVersion: NSFileProviderItemVersion, changedFields: NSFileProviderItemFields, contents newContents: URL?, options: NSFileProviderModifyItemOptions, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                var key = (item as? FileProviderItem)?.publicKey ?? item.itemIdentifier.rawValue
                // Rename/move first when the name changed, then content.
                if changedFields.contains(.filename) {
                    let parentKey = parentKeyForChild(item.parentItemIdentifier.rawValue)
                    let newKey = parentKey.isEmpty ? item.filename : parentKey + "/" + item.filename
                    try mount.move(key, to: newKey)
                    // H7: after the index re-point the OLD key is a
                    // tombstone - the content write and mark must target
                    // the NEW key or they strand bytes / resurrect the
                    // tombstone.
                    key = newKey
                }
                if let newContents {
                    let dst = try mount.cacheURLCreatingParents(ofKey: key)
                    if FileManager.default.fileExists(atPath: dst.path) {
                        try FileManager.default.removeItem(at: dst)
                    }
                    try FileManager.default.copyItem(at: newContents, to: dst)
                    try mount.mark(key)
                }
                let entry = try mount.item(key)
                progress.completedUnitCount = 100
                completionHandler(FileProviderItem(entry: entry, parentKey: parentKeyOf(entry.key)), [], false, nil)
            } catch {
                progress.completedUnitCount = 100
                completionHandler(nil, [], false, error)
            }
        }
        return progress
    }

    public func deleteItem(identifier: NSFileProviderItemIdentifier, baseVersion: NSFileProviderItemVersion, options: NSFileProviderDeleteItemOptions, request: NSFileProviderRequest, completionHandler: @escaping ((any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                try mount.delete(identifier.rawValue)
                progress.completedUnitCount = 100
                completionHandler(nil)
            } catch {
                progress.completedUnitCount = 100
                completionHandler(error)
            }
        }
        return progress
    }
}

// MARK: - key helpers

func parentKeyOf(_ key: String) -> String {
    // The parent path of a key: "" for top-level entries.
    guard let slash = key.lastIndex(of: "/") else { return "" }
    return String(key[key.startIndex..<slash])
}

/// F1: parent identifiers of nested items are DIR identifiers, which
/// carry a TRAILING slash (FileProviderItem.identifier(forDirKey:)
/// appends it when missing) - appending the separator without stripping
/// it produced double-slash child keys ("fp//doc.txt") that the daemon
/// stored literally and Enumerate never surfaced. The root container's
/// rawValue is a SENTINEL string ("NSFileProviderRootContainerItem..."),
/// not a path component - it maps to "" (top-level). Otherwise strips
/// ONE trailing '/'.
func parentKeyForChild(_ parent: String) -> String {
    if parent == NSFileProviderItemIdentifier.rootContainer.rawValue
        || parent == NSFileProviderItemIdentifier.workingSet.rawValue {
        return ""
    }
    return parent.hasSuffix("/") ? String(parent.dropLast()) : parent
}
