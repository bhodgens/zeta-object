// IPC.swift - the wire types of the zeta-cache local IPC protocol
// (protocol version 1), mirrored from zeta-cache/internal/ipc with
// Codable. COPIED from gui/macos/Sources/ZetaCacheMenu/IPC.swift and
// EXTENDED with the fileprovider-2026-10 leaf-01 types (enumerate /
// item / download / dehydrate / mark / delete / move).
//
// DUPLICATION RULE (documented, leaf-02 requirement 2): SPM cannot
// cross-import the gui/macos and gui/fileprovider packages, so the
// Codable structs are duplicated BY DESIGN. Both sides are pinned by
// the SAME golden fixtures (zeta-cache/internal/ipc/testdata/golden/*.json):
// the Go encoder's byte-exact responses. A fixture change that
// breaks either side fails that side's test suite. When you add a wire
// type: extend the Go structs, regenerate the fixtures (go test
// ./internal/ipc -run TestGoldenFixtures -update), then update BOTH
// Swift copies and add the new fixture tests on both sides.

import Foundation

/// Envelope version carried by every message.
enum IPCVersion {
    static let v1 = 1
}

// MARK: - Requests

/// One IPC request. `type` selects the handler; only the fields the
/// named type needs are set (they are omitted on the wire, matching the
/// Go encoder's `omitempty`).
struct IPCRequest: Encodable {
    var v: Int = IPCVersion.v1
    var type: String
    var path: String?
    var pin: Bool?
    var mode: String?
    var confirm: Bool?
    /// The move request's destination path (Go field `to`).
    var to: String?

    static func status() -> IPCRequest { IPCRequest(type: "status") }
    static func pins() -> IPCRequest { IPCRequest(type: "pins") }
    static func tombstones() -> IPCRequest { IPCRequest(type: "tombstones") }
    static func conflictsList() -> IPCRequest { IPCRequest(type: "conflicts.list") }
    static func deletedList() -> IPCRequest { IPCRequest(type: "deleted.list") }
    static func evict(_ path: String) -> IPCRequest { IPCRequest(type: "evict", path: path) }

    // fileprovider-2026-10 leaf-01 additions.
    static func enumerate(_ dirPath: String) -> IPCRequest {
        IPCRequest(type: "enumerate", path: dirPath)
    }
    static func item(_ path: String) -> IPCRequest { IPCRequest(type: "item", path: path) }
    static func download(_ path: String) -> IPCRequest { IPCRequest(type: "download", path: path) }
    static func dehydrate(_ path: String) -> IPCRequest { IPCRequest(type: "dehydrate", path: path) }
    static func mark(_ path: String) -> IPCRequest { IPCRequest(type: "mark", path: path) }
    static func delete(_ path: String) -> IPCRequest { IPCRequest(type: "delete", path: path) }
    static func move(_ path: String, to dest: String) -> IPCRequest {
        IPCRequest(type: "move", path: path, to: dest)
    }
}

// MARK: - Responses

/// The v1 envelope: {v, ok, data|error}. `data` carries the status
/// payload; `extra` carries every other payload exactly like the Go
/// encoder.
struct IPCResponse: Decodable {
    let v: Int
    let ok: Bool
    let data: StatusData?
    let extra: ExtraPayload?
    let error: String?
}

/// Decodes whatever `extra` holds, keyed by the request that produced it.
enum ExtraPayload: Decodable {
    case pins([String])
    case tombstones([Tombstone])
    case conflicts([ConflictItem])
    case resolve(ResolveAck)
    case restore(RestoreAck)
    case evict(EvictAck)
    // fileprovider-2026-10 leaf-01 additions.
    case enumerate(EnumerateData)
    case item(ItemData)
    case download(DownloadAck)
    case mark(MarkAck)
    case delete(DeleteAck)
    case move(MoveAck)

    private enum CodingKeys: String, CodingKey {
        case pins, tombstones, conflicts
        case path, mode, copyRemoved
        case entries, item, materialized, dirty, from, to
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        if let p = try c.decodeIfPresent([String].self, forKey: .pins) {
            self = .pins(p)
            return
        }
        if let t = try c.decodeIfPresent([Tombstone].self, forKey: .tombstones) {
            self = .tombstones(t)
            return
        }
        if let cf = try c.decodeIfPresent([ConflictItem].self, forKey: .conflicts) {
            self = .conflicts(cf)
            return
        }
        if let entries = try c.decodeIfPresent([FileProviderItemEntry].self, forKey: .entries) {
            self = .enumerate(EnumerateData(entries: entries))
            return
        }
        if let item = try c.decodeIfPresent(FileProviderItemEntry.self, forKey: .item) {
            self = .item(ItemData(item: item))
            return
        }
        if let dl = try c.decodeIfPresent(Bool.self, forKey: .materialized) {
            // A flat {"path","materialized","cachePath"} download ack:
            // re-decode the whole container as DownloadAck.
            let ack = try DownloadAck(from: decoder)
            self = .download(ack)
            return
        }
        if let dirty = try c.decodeIfPresent(Bool.self, forKey: .dirty),
           let path = try c.decodeIfPresent(String.self, forKey: .path) {
            self = .mark(MarkAck(path: path, dirty: dirty))
            return
        }
        if let from = try c.decodeIfPresent(String.self, forKey: .from),
           let to = try c.decodeIfPresent(String.self, forKey: .to) {
            self = .move(MoveAck(from: from, to: to))
            return
        }
        if let path = try c.decodeIfPresent(String.self, forKey: .path) {
            if let mode = try c.decodeIfPresent(String.self, forKey: .mode) {
                let removed = try c.decodeIfPresent(Bool.self, forKey: .copyRemoved) ?? false
                self = .resolve(ResolveAck(path: path, mode: mode, copyRemoved: removed))
                return
            }
            // A bare path ack is shared by restore / evict / delete;
            // the caller keys on the request it just sent.
            self = .delete(DeleteAck(path: path))
            return
        }
        throw DecodingError.dataCorrupted(
            .init(codingPath: decoder.codingPath, debugDescription: "unrecognized extra payload"))
    }
}

/// The status payload (data). Mirrors ipc.StatusData; unknown fields are
/// ignored so newer daemons stay compatible (additive fields only).
struct StatusData: Decodable {
    let state: String
    let server: String
    let bucket: String
    let lastSync: LastSyncValue?
    let dirty: Int
    let cached: Int
    let conflicts: Int
    let paused: Bool
    let usageBytes: Int
    let capBytes: Int
    let overflow: Int
    let evicted: Int
    let lastEvictAt: LastSyncValue?
    let lastError: String?
}

/// `lastSync` is a unix-seconds number or JSON null (never synced). Go's
/// `any` marshals both shapes; this wrapper accepts either.
struct LastSyncValue: Decodable {
    let unixSeconds: Int64?

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            unixSeconds = nil
        } else {
            unixSeconds = try? c.decode(Int64.self)
        }
    }
}

/// One tombstone row (tombstones / deleted.list).
struct Tombstone: Decodable {
    let path: String
    let deletedAt: Int64
    let expiresAt: Int64
}

/// One conflict record (conflicts.list).
struct ConflictItem: Decodable {
    let path: String
    let kind: String // "conflict-copy" | "kept-local"
    let copyPath: String?
}

struct ResolveAck: Decodable {
    let path: String
    let mode: String
    let copyRemoved: Bool
}

struct RestoreAck: Decodable {
    let path: String
}

struct EvictAck: Decodable {
    let path: String
}

// MARK: fileprovider-2026-10 leaf-01 payload types

/// One namespace row (enumerate entries / item payload). Mirrors
/// ipc.ItemEntry. `cachePath` is the backing file's path RELATIVE to the
/// daemon's cacheDir (slash-separated, "files/<key>"): the extension
/// opens the bytes directly at <cacheDir>/<cachePath> - data via the
/// FILE, metadata via IPC.
struct FileProviderItemEntry: Decodable {
    let name: String
    let key: String
    let isDir: Bool
    let size: Int64
    let mtime: Int64
    let materialized: Bool
    let dirty: Bool
    let cachePath: String
}

struct EnumerateData: Decodable {
    let entries: [FileProviderItemEntry]
}

struct ItemData: Decodable {
    let item: FileProviderItemEntry
}

/// The download (blocking hydrate) ack: {"path","materialized","cachePath"}.
struct DownloadAck: Decodable {
    let path: String
    let materialized: Bool
    let cachePath: String

    private enum CodingKeys: String, CodingKey {
        case path, materialized, cachePath
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        materialized = try c.decode(Bool.self, forKey: .materialized)
        cachePath = try c.decode(String.self, forKey: .cachePath)
    }
}

struct MarkAck: Decodable {
    let path: String
    let dirty: Bool
}

struct DeleteAck: Decodable {
    let path: String
}

struct MoveAck: Decodable {
    let from: String
    let to: String
}

// MARK: - Client

/// One-shot IPC client: connect, send ONE JSON request, read ONE JSON
/// response (the protocol's one-request-per-connection contract).
enum IPCClient {
    struct Failure: Error, LocalizedError {
        let message: String
        init(_ message: String) { self.message = message }
        var errorDescription: String? { message }
    }

    static func ask(_ socketPath: String, _ request: IPCRequest) throws -> IPCResponse {
        let sock = socket(AF_UNIX, SOCK_STREAM, 0)
        guard sock >= 0 else {
            throw Failure("socket() failed: errno \(errno)")
        }
        defer { close(sock) }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        // sun_path is 104 bytes on macOS; keep one byte for NUL.
        guard pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
            throw Failure("socket path too long: \(socketPath)")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { dest in
            dest.baseAddress!.copyMemory(from: pathBytes, byteCount: pathBytes.count)
        }
        let len = MemoryLayout<sockaddr_un>.size
        let connectResult = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                connect(sock, sa, socklen_t(len))
            }
        }
        guard connectResult == 0 else {
            throw Failure("cannot reach the daemon at \(socketPath) (errno \(errno))")
        }

        let encoder = JSONEncoder()
        var data = try encoder.encode(request)
        data.append(0x0A) // newline-terminated, like the Go test client
        guard writeAll(sock, data) else {
            throw Failure("failed writing the request (errno \(errno))")
        }

        // Read until EOF or a complete JSON line.
        var buffer = Data()
        let chunk = 4096
        var buf = [UInt8](repeating: 0, count: chunk)
        while true {
            let n = read(sock, &buf, chunk)
            if n <= 0 { break }
            buffer.append(contentsOf: buf[0..<n])
            if buffer.contains(0x0A) { break }
            if buffer.count > 1 << 20 { break } // sanity cap
        }
        guard !buffer.isEmpty else {
            throw Failure("the daemon closed the connection without a response")
        }
        let decoder = JSONDecoder()
        do {
            return try decoder.decode(IPCResponse.self, from: buffer)
        } catch {
            throw Failure("undecodable response: \(error)")
        }
    }

    private static func writeAll(_ fd: Int32, _ data: Data) -> Bool {
        var sent = 0
        let n = data.count
        return data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Bool in
            guard let base = raw.baseAddress else { return false }
            while sent < n {
                let w = write(fd, base.advanced(by: sent), n - sent)
                if w <= 0 { return false }
                sent += w
            }
            return true
        }
    }
}
