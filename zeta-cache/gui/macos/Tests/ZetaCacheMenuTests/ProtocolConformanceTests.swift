// ProtocolConformanceTests.swift - the Swift half of the golden-file
// protocol conformance (leaf-08 acceptance). The SAME JSON fixtures the
// Go encoder's responses were pinned to (zeta-cache/internal/ipc/
// testdata/golden/*.json) are decoded here with the app's Codable
// types. If both sides decode every fixture, the GUI can read every
// response shape the daemon emits.

import XCTest
@testable import ZetaCacheMenu

final class ProtocolConformanceTests: XCTestCase {

    private func fixture(_ name: String) throws -> Data {
        try Fixtures.data(name)
    }

    private func decode(_ name: String, file: StaticString = #filePath, line: UInt = #line) throws -> IPCResponse {
        let data = try fixture(name)
        do {
            return try JSONDecoder().decode(IPCResponse.self, from: data)
        } catch {
            XCTFail("\(name): decode failed: \(error)", file: file, line: line)
            throw error
        }
    }

    func testStatusFixture() throws {
        let resp = try decode("status")
        XCTAssertTrue(resp.ok)
        XCTAssertEqual(resp.v, 1)
        let data = try XCTUnwrap(resp.data)
        XCTAssertEqual(data.state, "idle")
        XCTAssertEqual(data.server, "https://cache.example")
        XCTAssertEqual(data.bucket, "bkt")
        XCTAssertNil(data.lastSync?.unixSeconds)
        XCTAssertEqual(data.dirty, 0)
        XCTAssertEqual(data.conflicts, 0)
        XCTAssertFalse(data.paused)
        XCTAssertEqual(data.usageBytes, 0)
        XCTAssertEqual(data.capBytes, 0)
        XCTAssertNil(data.lastError)
    }

    func testPauseResumeFixtures() throws {
        let paused = try decode("pause")
        XCTAssertTrue(paused.ok)
        XCTAssertTrue(try XCTUnwrap(paused.data).paused)
        let resumed = try decode("resume")
        XCTAssertFalse(try XCTUnwrap(resumed.data).paused)
    }

    func testPinAndPinsFixtures() throws {
        let pin = try decode("pin")
        XCTAssertTrue(pin.ok)
        guard case .pins(let pins) = pin.extra else {
            return XCTFail("pin: extra is not pins")
        }
        XCTAssertTrue(pins.contains("docs/keep.txt"))

        let list = try decode("pins")
        guard case .pins(let listed) = list.extra else {
            return XCTFail("pins: extra is not pins")
        }
        XCTAssertEqual(listed, ["docs/keep.txt", "photos/raws"])
    }

    func testTombstonesFixture() throws {
        let resp = try decode("tombstones")
        guard case .tombstones(let tombs) = resp.extra else {
            return XCTFail("tombstones: extra is not tombstones")
        }
        XCTAssertEqual(tombs.first?.path, "notes/old.txt")
        XCTAssertEqual(tombs.first?.deletedAt, 1_728_211_200)
        XCTAssertEqual(tombs.first?.expiresAt, 1_730_803_200)
    }

    func testConflictsListFixture() throws {
        let resp = try decode("conflicts-list")
        guard case .conflicts(let conflicts) = resp.extra else {
            return XCTFail("conflicts-list: extra is not conflicts")
        }
        XCTAssertEqual(conflicts.count, 2)
        XCTAssertEqual(conflicts[0].path, "doc.txt")
        XCTAssertEqual(conflicts[0].kind, "conflict-copy")
        XCTAssertEqual(conflicts[0].copyPath, "doc (conflicted copy 2026-10-07).txt")
        XCTAssertEqual(conflicts[1].kind, "kept-local")
        XCTAssertNil(conflicts[1].copyPath)
    }

    func testConflictsResolveFixture() throws {
        let resp = try decode("conflicts-resolve")
        guard case .resolve(let ack) = resp.extra else {
            return XCTFail("conflicts-resolve: extra is not a resolve ack")
        }
        XCTAssertEqual(ack.path, "doc.txt")
        XCTAssertEqual(ack.mode, "keep-local")
        XCTAssertTrue(ack.copyRemoved)
    }

    func testDeletedListAndRestoreFixtures() throws {
        let list = try decode("deleted-list")
        guard case .tombstones(let tombs) = list.extra else {
            return XCTFail("deleted-list: extra is not tombstones")
        }
        XCTAssertEqual(tombs.first?.path, "notes/old.txt")

        let restore = try decode("deleted-restore")
        guard case .restore(let ack) = restore.extra else {
            return XCTFail("deleted-restore: extra is not a restore ack")
        }
        XCTAssertEqual(ack.path, "notes/old.txt")
    }

    func testEvictFixture() throws {
        let resp = try decode("evict")
        guard case .restore(let ack) = resp.extra else {
            // evict's extra is {path}; it decodes through the same
            // single-path branch as restore.
            return XCTFail("evict: extra did not decode as a path ack")
        }
        XCTAssertEqual(ack.path, "cache/big.bin")
    }

    func testErrorFixtures() throws {
        for name in ["unknown-type", "bad-version", "resolve-missing-mode",
                     "restore-missing-path", "evict-missing-path"] {
            let resp = try decode(name)
            XCTAssertFalse(resp.ok, "\(name): ok should be false")
            XCTAssertNil(resp.data, "\(name): data should be nil")
            XCTAssertNotNil(resp.error, "\(name): error string missing")
        }
        XCTAssertEqual(try decode("unknown-type").error, "unknown request type")
        XCTAssertTrue(try decode("bad-version").error!.contains("unsupported protocol version"))
    }

    /// The GUI's request encoder must produce the shapes the daemon's
    /// decoder expects (field names + omitempty semantics). JSON object
    /// key ORDER is not significant (the daemon's decoder is a map), so
    /// the comparison is structural: decode both and re-encode.
    func testRequestEncoding() throws {
        func canonical(_ data: Data) throws -> String {
            let obj = try JSONSerialization.jsonObject(with: data)
            let sorted = try JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys])
            return String(data: sorted, encoding: .utf8)!
        }
        func assertShape(_ request: IPCRequest, _ want: String,
                         file: StaticString = #filePath, line: UInt = #line) throws {
            let got = try canonical(JSONEncoder().encode(request))
            let expected = try canonical(want.data(using: .utf8)!)
            XCTAssertEqual(got, expected, file: file, line: line)
        }

        try assertShape(.status(), "{\"v\":1,\"type\":\"status\"}")
        try assertShape(
            .conflictsResolve("doc.txt", .keepLocal, confirm: true),
            "{\"v\":1,\"type\":\"conflicts.resolve\",\"path\":\"doc.txt\",\"mode\":\"keep-local\",\"confirm\":true}")
        try assertShape(.pin("a.txt", true), "{\"v\":1,\"type\":\"pin\",\"path\":\"a.txt\",\"pin\":true}")
        try assertShape(.deletedRestore("g.txt"), "{\"v\":1,\"type\":\"deleted.restore\",\"path\":\"g.txt\"}")
        try assertShape(.evict("c.bin"), "{\"v\":1,\"type\":\"evict\",\"path\":\"c.bin\"}")

        // omitempty: absent fields must NOT appear on the wire.
        let raw = String(data: try JSONEncoder().encode(IPCRequest.status()), encoding: .utf8)!
        XCTAssertFalse(raw.contains("path"), "status must omit path")
        XCTAssertFalse(raw.contains("mode"), "status must omit mode")
        XCTAssertFalse(raw.contains("pin"), "status must omit pin")
    }

    /// Additive-forward-compat property: unknown fields in the envelope
    /// and status payload are ignored (v2 daemons must not break v1 GUIs).
    func testUnknownFieldsTolerated() throws {
        let json = """
        {"v":1,"ok":true,"data":{"state":"idle","server":"s","bucket":"b",
         "lastSync":1728211200,"dirty":1,"cached":2,"conflicts":3,"paused":false,
         "usageBytes":10,"capBytes":100,"overflow":0,"evicted":4,"lastEvictAt":null,
         "someFutureField":{"nested":true}},"extra":{"pins":["x"],"future":"y"}}
        """.data(using: .utf8)!
        let resp = try JSONDecoder().decode(IPCResponse.self, from: json)
        XCTAssertEqual(resp.data?.lastSync?.unixSeconds, 1_728_211_200)
        XCTAssertEqual(resp.data?.dirty, 1)
        guard case .pins(let pins) = resp.extra else {
            return XCTFail("extra with unknown sibling fields must still decode pins")
        }
        XCTAssertEqual(pins, ["x"])
    }
}
