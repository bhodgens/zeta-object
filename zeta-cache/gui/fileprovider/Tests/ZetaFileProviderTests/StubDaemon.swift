// StubDaemon.swift - an in-process stub of the zeta-cache IPC daemon:
// a unix socket answering one JSON response per connection from a
// canned table. Used by the download-trigger happy-path test and the
// error-mapping tests (fileprovider-2026-10 leaf 02 acceptance 3).

import Foundation
@testable import ZetaFileProvider

final class StubIPCServer {
    let path: String
    private var responses: [String: String] = [:] // request type -> raw JSON
    private let queue = DispatchQueue(label: "stub-ipc")
    private var serverFD: Int32 = -1
    private var stopped = false

    init(path: String) throws {
        self.path = path
        try? FileManager.default.removeItem(atPath: path)
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw IPCClient.Failure("stub socket() failed: errno \(errno)")
        }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
            close(fd)
            throw IPCClient.Failure("stub socket path too long")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { dest in
            dest.baseAddress!.copyMemory(from: bytes, byteCount: bytes.count)
        }
        let bindResult = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                bind(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bindResult == 0 else {
            close(fd)
            throw IPCClient.Failure("stub bind failed: errno \(errno)")
        }
        guard listen(fd, 8) == 0 else {
            close(fd)
            throw IPCClient.Failure("stub listen failed: errno \(errno)")
        }
        serverFD = fd
        queue.async { [weak self] in self?.acceptLoop() }
    }

    deinit { stop() }

    /// Registers the raw JSON response the stub answers for a type.
    func respond(to type: String, with json: String) {
        responses[type] = json
    }

    func stop() {
        guard !stopped else { return }
        stopped = true
        if serverFD >= 0 {
            close(serverFD)
            serverFD = -1
        }
        try? FileManager.default.removeItem(atPath: path)
    }

    private func acceptLoop() {
        while !stopped {
            let conn = accept(serverFD, nil, nil)
            guard conn >= 0 else { return }
            let line = readLine(conn)
            defer { close(conn) }
            guard let line, let data = line.data(using: .utf8),
                  let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let type = obj["type"] as? String,
                  let resp = responses[type] else {
                _ = writeAll(conn, Data("{\"v\":1,\"ok\":false,\"error\":\"stub: no canned response\"}\n".utf8))
                continue
            }
            _ = writeAll(conn, Data(resp.utf8))
        }
    }

    private func readLine(_ fd: Int32) -> String? {
        var buf = Data()
        var chunk = [UInt8](repeating: 0, count: 4096)
        while true {
            let n = read(fd, &chunk, 4096)
            if n <= 0 { break }
            buf.append(contentsOf: chunk[0..<n])
            if buf.contains(0x0A) { break }
        }
        guard !buf.isEmpty else { return nil }
        return String(decoding: buf, as: UTF8.self)
    }

    private func writeAll(_ fd: Int32, _ data: Data) -> Bool {
        data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Bool in
            guard let base = raw.baseAddress else { return false }
            var sent = 0
            while sent < data.count {
                let w = write(fd, base.advanced(by: sent), data.count - sent)
                if w <= 0 { return false }
                sent += w
            }
            return true
        }
    }
}
