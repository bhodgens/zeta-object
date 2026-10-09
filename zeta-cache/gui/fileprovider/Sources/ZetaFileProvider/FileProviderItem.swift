// FileProviderItem.swift - the NSFileProviderItem conformance mapped
// from an IPC ItemEntry (fileprovider-2026-10 leaf 02). The item
// identity IS the server key (stable across materialization and moves;
// the daemon's index owns the key -> path relation).
//
// Content policy: materialized -> .always (bytes are on disk); not
// materialized -> .downloadsWhenOpened (Finder triggers the extension's
// download path on first open). Dirty items are .always too - their
// bytes are local by definition (the flag's semantics are surfaced via
// isUploaded/isUploading below, not by withholding the bytes).

import FileProvider
import Foundation
import UniformTypeIdentifiers

/// One zeta-cache namespace row surfaced as a FileProvider item.
/// Constructed ONLY from FileProviderItemEntry (the IPC entry), so the
/// mapping table in the leaf-02 report is enforced by construction.
public final class FileProviderItem: NSObject, NSFileProviderItem {
    let entry: FileProviderItemEntry
    /// The parent item identifier: "." (root) or a dir key's hash.
    let parentIdentifier: NSFileProviderItemIdentifier

    init(entry: FileProviderItemEntry, parentKey: String) {
        self.entry = entry
        self.parentIdentifier = FileProviderItem.identifier(forDirKey: parentKey)
        super.init()
    }

    // MARK: identity

    /// The domain-relative identity of a dir key: "." for the root,
    /// otherwise the trailing-slash-stripped key. File keys are used
    /// verbatim (the server key). Keys are path-shaped and unique in the
    /// daemon's index, so the string is its own stable identifier.
    public static func identifier(for key: String) -> NSFileProviderItemIdentifier {
        if key.isEmpty || key == "/" {
            return .rootContainer
        }
        return NSFileProviderItemIdentifier(key)
    }

    /// The dir-key form of a parent ("a/b" -> "a/b/", "" -> root).
    static func identifier(forDirKey key: String) -> NSFileProviderItemIdentifier {
        if key.isEmpty {
            return .rootContainer
        }
        return NSFileProviderItemIdentifier(key.hasSuffix("/") ? key : key + "/")
    }

    /// The dir key of a file key's parent ("a/b.txt" -> "a/b/";
    /// top-level -> root).
    static func parentDirKey(of key: String) -> String {
        guard let idx = key.lastIndex(of: "/") else { return "" }
        return String(key[...idx])
    }

    /// The server key backing an identifier (reverse of the above).
    static func dirKey(of identifier: NSFileProviderItemIdentifier) -> String {
        if identifier == .rootContainer || identifier == .workingSet {
            return ""
        }
        let raw = identifier.rawValue
        return raw.hasSuffix("/") ? raw : raw + "/"
    }

    // MARK: NSFileProviderItem

    public var itemIdentifier: NSFileProviderItemIdentifier {
        NSFileProviderItemIdentifier(entry.key)
    }

    public var parentItemIdentifier: NSFileProviderItemIdentifier {
        parentIdentifier
    }

    public var filename: String { entry.name }

    /// The type: folders are .folder; files declare a plain data type
    /// (the daemon's cache file is opaque bytes; UTI refinement is a
    /// leaf-03 polish).
    public var contentType: UTType {
        entry.isDir ? .folder : .data
    }

    public var documentSize: NSNumber? {
        entry.isDir ? nil : NSNumber(value: entry.size)
    }

    /// Content policy per the locked leaf-02 mapping: materialized ->
    /// keep the bytes locally (.downloadEagerlyAndKeepDownloaded, the
    /// SDK's ".always" semantic); not materialized -> hydrate on open
    /// (.downloadLazily, the ".downloadsWhenOpened" semantic).
    public var contentPolicy: NSFileProviderContentPolicy {
        if entry.materialized || entry.dirty {
            return .downloadEagerlyAndKeepDownloaded
        }
        return .downloadLazily
    }

    public var creationDate: Date? {
        date(unix: entry.mtime)
    }

    public var contentModificationDate: Date? {
        date(unix: entry.mtime)
    }

    /// Dirty = unsynced local edits: uploaded=false, uploadedOnce reflects
    /// whether a known-good ETag generation exists. Clean items count as
    /// uploaded (the server copy is authoritative).
    public var isUploaded: Bool { !entry.dirty }

    public var isUploading: Bool { false }

    public var isDownloaded: Bool { entry.materialized }

    public var isDownloading: Bool { false }

    public var isShared: Bool { false }

    /// Dehydration surface (NSFileProviderDehydratableItem): every clean
    /// materialized file can be dehydrated via IPC dehydrate; dirty files
    /// cannot (the daemon refuses).
    public var isDehydratable: Bool {
        entry.materialized && !entry.dirty && !entry.isDir
    }

    private func date(unix: Int64) -> Date? {
        guard unix > 0 else { return nil }
        return Date(timeIntervalSince1970: TimeInterval(unix))
    }
}

// MARK: - Error mapping

/// Maps a daemon IPC error string to the FileProvider error the Finder
/// understands. The daemon's not-found shape is "no such item: <path>"
/// (leaf-01 handler); everything else surfaces as .notAKnownItem's
/// sibling: a plain local error with the message (the honest EIO analog).
public enum FileProviderErrorMapper {
    public static func error(fromIPC message: String) -> NSError {
        if message.hasPrefix("no such item") {
            return NSError(
                domain: NSFileProviderErrorDomain,
                code: Int(NSFileProviderError.noSuchItem.rawValue),
                userInfo: [NSLocalizedDescriptionKey: message])
        }
        return NSError(
            domain: NSFileProviderErrorDomain,
            code: Int(NSFileProviderError.serverUnreachable.rawValue),
            userInfo: [NSLocalizedDescriptionKey: message])
    }
}
