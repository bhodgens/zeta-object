// IPC.swift - the wire types of the zeta-cache local IPC protocol
// (protocol version 1), mirrored from zeta-cache/internal/ipc with
// Codable. The GUI speaks ONLY this: one JSON request per connection on
// a Unix domain socket (mode 0600, user's runtime dir).

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

    static func status() -> IPCRequest { IPCRequest(type: "status") }
    static func pause() -> IPCRequest { IPCRequest(type: "pause") }
    static func resume() -> IPCRequest { IPCRequest(type: "resume") }
    static func pin(_ path: String, _ pinned: Bool) -> IPCRequest {
        IPCRequest(type: "pin", path: path, pin: pinned)
    }
    static func pins() -> IPCRequest { IPCRequest(type: "pins") }
    static func tombstones() -> IPCRequest { IPCRequest(type: "tombstones") }
    static func conflictsList() -> IPCRequest { IPCRequest(type: "conflicts.list") }
    static func conflictsResolve(_ path: String, _ mode: ResolveMode, confirm: Bool) -> IPCRequest {
        IPCRequest(type: "conflicts.resolve", path: path, mode: mode.rawValue, confirm: confirm)
    }
    static func deletedList() -> IPCRequest { IPCRequest(type: "deleted.list") }
    static func deletedRestore(_ path: String) -> IPCRequest {
        IPCRequest(type: "deleted.restore", path: path)
    }
    static func evict(_ path: String) -> IPCRequest { IPCRequest(type: "evict", path: path) }
}

/// Conflict resolution modes.
enum ResolveMode: String {
    case keepLocal = "keep-local"
    case keepRemote = "keep-remote"
}

// MARK: - Responses

/// The v1 envelope: {v, ok, data|error}. `data` carries the status
/// payload; `extra` carries every other payload (pins, tombstones,
/// conflicts, resolve/restore/evict acks) exactly like the Go encoder.
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

    private enum CodingKeys: String, CodingKey {
        case pins, tombstones, conflicts
        case path, mode, copyRemoved
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
        if let path = try c.decodeIfPresent(String.self, forKey: .path) {
            if let mode = try c.decodeIfPresent(String.self, forKey: .mode) {
                let removed = try c.decodeIfPresent(Bool.self, forKey: .copyRemoved) ?? false
                self = .resolve(ResolveAck(path: path, mode: mode, copyRemoved: removed))
                return
            }
            self = .restore(RestoreAck(path: path))
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
