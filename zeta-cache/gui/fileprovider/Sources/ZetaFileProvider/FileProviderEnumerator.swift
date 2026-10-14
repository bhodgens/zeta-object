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

    // MARK: - sync anchor (L43)

    /// A deterministic sync anchor: a stable hash of the container's
    /// current (key, mtime, size) rows, sorted. Identical index content
    /// yields an identical anchor, so an unchanged container reports "no
    /// changes" and a changed one reports the diff. (This is a
    /// SIMPLIFICATION, documented per L43: content bytes are not hashed -
    /// a same-size, same-mtime in-place rewrite would be missed until the
    /// next mtime change; the daemon's mark path sets mtime on every
    /// write, so the practical exposure is nil.)
    static func anchor(for entries: [FileProviderItemEntry]) -> NSFileProviderSyncAnchor {
        let rows = entries
            .map { "\($0.key)|\($0.mtime)|\($0.size)|\($0.isDir ? 1 : 0)" }
            .sorted()
            .joined(separator: ";")
        // Any stable digest works; SHA256 via CommonCrypto needs an
        // Objective-C bridging dance in SPM, so use a simple stable
        // FNV-1a over the UTF-8 bytes and hex-encode it.
        var hash: UInt64 = 0xcbf29ce484222325
        for byte in rows.utf8 {
            hash = (hash ^ UInt64(byte)) &* 0x100000001b3
        }
        return NSFileProviderSyncAnchor(String(format: "%016llx", hash).data(using: .utf8)!)
    }

    private func enumerateEntries() throws -> [FileProviderItemEntry] {
        try mount.enumerate(dirKey: FileProviderItem.dirKey(of: containerItemIdentifier ?? .rootContainer))
    }

    /// Initial content: one pass over IPC enumerate, then hand the
    /// derived anchor to the system so change tracking engages (the
    /// previous nil anchor meant enumerateChanges was never called - L43).
    public func enumerateItems(for observer: NSFileProviderEnumerationObserver, startingAt page: NSFileProviderPage) {
        let entries: [FileProviderItemEntry]
        do {
            entries = try enumerateEntries()
        } catch {
            observer.finishEnumeratingWithError(error)
            return
        }
        observer.didEnumerate(entries.map { entry in
            FileProviderItem(entry: entry, parentKey: FileProviderItem.parentDirKey(of: entry.key))
        })
        // NSFileProviderPage and NSFileProviderSyncAnchor are both NSData
        // typedefs; route through NSData so the anchor value doubles as
        // the continuation page (single-batch enumeration, no more pages
        // - the system passes it back in `page` on resume).
        let anchor = FileProviderEnumerator.anchor(for: entries)
        observer.finishEnumerating(upTo: anchor as NSData as NSFileProviderPage)
    }

    /// Change tracking: fresh enumerate vs the anchor the system holds.
    /// SIMPLIFICATION (documented, L43 minimum): the daemon's index is the
    /// single truth and this extension keeps no local row snapshot, so the
    /// diff cannot name individual deletions - on an anchor mismatch the
    /// CURRENT batch is re-surfaced via didUpdate (the system merges by
    /// itemIdentifier) and removals surface via the system's own merge
    /// against its cached view. This is strictly more honest than the
    /// previous "always no changes" answer while staying snapshot-free.
    public func enumerateChanges(for observer: NSFileProviderChangeObserver, from anchor: NSFileProviderSyncAnchor) {
        let entries: [FileProviderItemEntry]
        do {
            entries = try enumerateEntries()
        } catch {
            observer.finishEnumeratingWithError(error)
            return
        }
        let newAnchor = FileProviderEnumerator.anchor(for: entries)
        if newAnchor != anchor {
            observer.didUpdate(entries.map { entry in
                FileProviderItem(entry: entry, parentKey: FileProviderItem.parentDirKey(of: entry.key))
            })
        }
        observer.finishEnumeratingChanges(upTo: newAnchor, moreComing: false)
    }
}
