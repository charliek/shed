import Foundation
import XCTest

@testable import ShedKit

final class IPCMessagesTests: XCTestCase {
    func testIPCRequestRejectsUnknownTopLevelKeys() throws {
        let json = #"{"id":"1","op":"ping","extra":true}"#
        XCTAssertThrowsError(try JSONDecoder().decode(IPCRequest.self, from: Data(json.utf8))) { err in
            guard case DecodingError.dataCorrupted(let ctx) = err else {
                return XCTFail("expected dataCorrupted, got \(err)")
            }
            XCTAssertTrue(ctx.debugDescription.contains("extra"), ctx.debugDescription)
        }
    }

    func testIPCRequestAcceptsKnownKeys() throws {
        let json = #"{"id":"42","op":"ping"}"#
        let req = try JSONDecoder().decode(IPCRequest.self, from: Data(json.utf8))
        XCTAssertEqual(req.id, 42)
        XCTAssertEqual(req.op, "ping")
        XCTAssertNil(req.params)
    }
}
