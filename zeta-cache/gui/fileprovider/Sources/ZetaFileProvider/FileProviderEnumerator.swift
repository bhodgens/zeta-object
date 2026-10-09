// FileProviderEnumerator.swift - enumerates a container via the daemon's
// IPC `enumerate` (fileprovider-2026-10 leaf 01) and hands Finder
// FileProviderItems. The daemon's index IS the namespace truth (the
// sync's PROPFIND-cached view); this class never touches the network.

import FileProvider
import Foundation

public final class FileProviderEnumerator: NSObject, NSFileProviderEnumerator {
    /// The container being enumerated: nil = the root container.
    let containerItemIdentifier: NSFileProviderItemIdentifier?
    let mount: FileProviderMount

    public init(containerItemIdentifier: NSFileProviderItemIdentifier?, mount: FileProviderMount) {
        self.containerItemIdentifier = containerItemIdentifier
        self.mount = mount
        super.init()
    }

    public func invalidate() {}

    /// Initial content: one pass over IPC enumerate, then done. The
    /// daemon's sync keeps the index fresh; changes re-surface via the
    /// next enumeration (a full delta surface is leaf-03+ work).
    public func enumerateItems(for observer: NSFileProviderEnumerationObserver, startingAt page: NSFileProviderPage) {
        let dirKey = FileProviderItem.dirKey(of: containerItemIdentifier ?? .rootContainer)
        do {
            let entries = try mount.enumerate(dirKey: dirKey)
            observer.didEnumerate(entries.map { entry in
                FileProviderItem(entry: entry, parentKey: dirKey)
            })
        } catch {
            observer.finishEnumeratingWithError(error)
            return
        }
        observer.finishEnumerating(upTo: nil)
    }

    public func enumerateChanges(for observer: NSFileProviderChangeObserver, from anchor: NSFileProviderSyncAnchor) {
        // v1: report a full re-enumeration (delete everything + re-add)
        // is WORSE than reporting nothing; the anchor-less honest answer
        // is "no changes since your anchor" - Finder re-enumerates on
        // focus anyway.
        observer.finishEnumeratingChanges(upTo: anchor, moreComing: false)
    }
}
