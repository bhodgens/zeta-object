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

final class ReplicatedExtension: NSObject, NSFileProviderReplicatedExtension {
    let domain: NSFileProviderDomain
    let mount: FileProviderMount

    required init(domain: NSFileProviderDomain) {
        self.domain = domain
        self.mount = .standard()
        super.init()
    }

    init(domain: NSFileProviderDomain, mount: FileProviderMount) {
        // Test seam: inject a stub-backed mount.
        self.domain = domain
        self.mount = mount
        super.init()
    }

    func invalidate() {
        // No persistent state: IPC connections are per-request.
    }

    // MARK: - Enumeration (NSFileProviderEnumerating)

    func enumerator(for containerItemIdentifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest) throws -> NSFileProviderEnumerator {
        FileProviderEnumerator(containerItemIdentifier: containerItemIdentifier, mount: mount)
    }

    // MARK: - Items

    func item(for identifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, (any Error)?) -> Void) -> Progress {
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

    func fetchContents(for itemIdentifier: NSFileProviderItemIdentifier, version requestedVersion: NSFileProviderItemVersion?, request: NSFileProviderRequest, completionHandler: @escaping (URL?, NSFileProviderItem?, (any Error)?) -> Void) -> Progress {
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

    func createItem(basedOn template: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?, options: NSFileProviderCreateItemOptions, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                let parent = template.parentItemIdentifier.rawValue
                let key = parent == NSFileProviderItemIdentifier.rootContainer.rawValue
                    ? template.filename
                    : parent + "/" + template.filename
                if let url {
                    try FileManager.default.copyItem(at: url, to: mount.cacheURL(ofKey: key))
                } else {
                    try FileManager.default.createDirectory(at: mount.cacheURL(ofKey: key), withIntermediateDirectories: true)
                }
                try mount.mark(key)
                let entry = try mount.item(key)
                progress.completedUnitCount = 100
                // The index is authoritative for every field: nothing left
                // for the system to fetch back (inducted=false).
                completionHandler(FileProviderItem(entry: entry, parentKey: parentKeyOf(entry.key)), [], false, nil)
            } catch {
                progress.completedUnitCount = 100
                completionHandler(nil, [], false, error)
            }
        }
        return progress
    }

    func modifyItem(_ item: NSFileProviderItem, baseVersion: NSFileProviderItemVersion, changedFields: NSFileProviderItemFields, contents newContents: URL?, options: NSFileProviderModifyItemOptions, request: NSFileProviderRequest, completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void) -> Progress {
        let progress = Progress(totalUnitCount: 100)
        Task {
            do {
                let key = (item as? FileProviderItem)?.publicKey ?? item.itemIdentifier.rawValue
                // Rename/move first when the name changed, then content.
                if changedFields.contains(.filename) {
                    let parent = item.parentItemIdentifier.rawValue
                    let newKey = parent == NSFileProviderItemIdentifier.rootContainer.rawValue
                        ? item.filename
                        : parent + "/" + item.filename
                    try mount.move(key, to: newKey)
                }
                if let newContents {
                    try FileManager.default.copyItem(at: newContents, to: mount.cacheURL(ofKey: key))
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

    func deleteItem(identifier: NSFileProviderItemIdentifier, baseVersion: NSFileProviderItemVersion, options: NSFileProviderDeleteItemOptions, request: NSFileProviderRequest, completionHandler: @escaping ((any Error)?) -> Void) -> Progress {
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
